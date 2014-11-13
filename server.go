// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
// See https://code.google.com/p/go/source/browse/CONTRIBUTORS
// Licensed under the same terms as Go itself:
// https://code.google.com/p/go/source/browse/LICENSE

package http2

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bradfitz/http2/hpack"
)

// TODO: finish GOAWAY support. Consider each incoming frame type and whether
// it should be ignored during a shutdown race.

// Server is an HTTP/2 server.
type Server struct {
	// MaxStreams optionally ...
	MaxStreams int
}

var testHookOnConn func() // for testing

// ConfigureServer adds HTTP/2 support to a net/http Server.
//
// The configuration conf may be nil.
//
// ConfigureServer must be called before s begins serving.
func ConfigureServer(s *http.Server, conf *Server) {
	if conf == nil {
		conf = new(Server)
	}
	if s.TLSConfig == nil {
		s.TLSConfig = new(tls.Config)
	}
	haveNPN := false
	for _, p := range s.TLSConfig.NextProtos {
		if p == npnProto {
			haveNPN = true
			break
		}
	}
	if !haveNPN {
		s.TLSConfig.NextProtos = append(s.TLSConfig.NextProtos, npnProto)
	}

	if s.TLSNextProto == nil {
		s.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	}
	s.TLSNextProto[npnProto] = func(hs *http.Server, c *tls.Conn, h http.Handler) {
		if testHookOnConn != nil {
			testHookOnConn()
		}
		conf.handleConn(hs, c, h)
	}
}

func (srv *Server) handleConn(hs *http.Server, c net.Conn, h http.Handler) {
	sc := &serverConn{
		hs:                hs,
		conn:              c,
		handler:           h,
		framer:            NewFramer(c, c), // TODO: write to a (custom?) buffered writer that can alternate when it's in buffered mode.
		streams:           make(map[uint32]*stream),
		canonHeader:       make(map[string]string),
		readFrameCh:       make(chan frameAndProcessed),
		readFrameErrCh:    make(chan error, 1),
		writeHeaderCh:     make(chan headerWriteReq), // must not be buffered
		windowUpdateCh:    make(chan windowUpdateReq, 8),
		flow:              newFlow(initialWindowSize),
		doneServing:       make(chan struct{}),
		maxWriteFrameSize: initialMaxFrameSize,
		initialWindowSize: initialWindowSize,
		serveG:            newGoroutineLock(),
	}
	sc.hpackEncoder = hpack.NewEncoder(&sc.headerWriteBuf)
	sc.hpackDecoder = hpack.NewDecoder(initialHeaderTableSize, sc.onNewHeaderField)
	sc.serve()
}

// frameAndProcessed coordinates the readFrames and serve goroutines, since
// the Framer interface only permits the most recently-read Frame from being
// accessed. The serve goroutine sends on processed to signal to the readFrames
// goroutine that another frame may be read.
type frameAndProcessed struct {
	f         Frame
	processed chan struct{}
}

type serverConn struct {
	// Immutable:
	hs             *http.Server
	conn           net.Conn
	handler        http.Handler
	framer         *Framer
	hpackDecoder   *hpack.Decoder
	hpackEncoder   *hpack.Encoder
	doneServing    chan struct{}          // closed when serverConn.serve ends
	readFrameCh    chan frameAndProcessed // written by serverConn.readFrames
	readFrameErrCh chan error
	writeHeaderCh  chan headerWriteReq // must not be buffered
	windowUpdateCh chan windowUpdateReq
	serveG         goroutineLock // used to verify funcs are on serve()
	flow           *flow         // the connection-wide one

	// Everything following is owned by the serve loop; use serveG.check()
	maxStreamID       uint32 // max ever seen
	streams           map[uint32]*stream
	maxWriteFrameSize uint32 // TODO: update this when settings come in
	initialWindowSize int32
	canonHeader       map[string]string // http2-lower-case -> Go-Canonical-Case
	sentGoAway        bool
	req               requestParam // non-zero while reading request headers
	headerWriteBuf    bytes.Buffer // used to write response headers
}

// requestParam is the state of the next request, initialized over
// potentially several frames HEADERS + zero or more CONTINUATION
// frames.
type requestParam struct {
	// stream is non-nil if we're reading (HEADER or CONTINUATION)
	// frames for a request (but not DATA).
	stream            *stream
	header            http.Header
	method, path      string
	scheme, authority string
	sawRegularHeader  bool // saw a non-pseudo header already
	invalidHeader     bool // an invalid header was seen
}

type stream struct {
	id    uint32
	state streamState // owned by serverConn's processing loop
	flow  *flow       // limits writing from Handler to client
	body  *pipe       // non-nil if expecting DATA frames

	bodyBytes     int64 // body bytes seen so far
	declBodyBytes int64 // or -1 if undeclared
}

func (sc *serverConn) state(streamID uint32) streamState {
	sc.serveG.check()
	// http://http2.github.io/http2-spec/#rfc.section.5.1
	if st, ok := sc.streams[streamID]; ok {
		return st.state
	}
	// "The first use of a new stream identifier implicitly closes all
	// streams in the "idle" state that might have been initiated by
	// that peer with a lower-valued stream identifier. For example, if
	// a client sends a HEADERS frame on stream 7 without ever sending a
	// frame on stream 5, then stream 5 transitions to the "closed"
	// state when the first frame for stream 7 is sent or received."
	if streamID <= sc.maxStreamID {
		return stateClosed
	}
	return stateIdle
}

func (sc *serverConn) vlogf(format string, args ...interface{}) {
	if VerboseLogs {
		sc.logf(format, args...)
	}
}

func (sc *serverConn) logf(format string, args ...interface{}) {
	if lg := sc.hs.ErrorLog; lg != nil {
		lg.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

func (sc *serverConn) condlogf(err error, format string, args ...interface{}) {
	if err == nil {
		return
	}
	str := err.Error()
	if strings.Contains(str, "use of closed network connection") {
		// Boring, expected errors.
		sc.vlogf(format, args...)
	} else {
		sc.logf(format, args...)
	}
}

func (sc *serverConn) onNewHeaderField(f hpack.HeaderField) {
	sc.serveG.check()
	switch {
	case !validHeader(f.Name):
		sc.req.invalidHeader = true
	case strings.HasPrefix(f.Name, ":"):
		if sc.req.sawRegularHeader {
			sc.logf("pseudo-header after regular header")
			sc.req.invalidHeader = true
			return
		}
		var dst *string
		switch f.Name {
		case ":method":
			dst = &sc.req.method
		case ":path":
			dst = &sc.req.path
		case ":scheme":
			dst = &sc.req.scheme
		case ":authority":
			dst = &sc.req.authority
		default:
			// 8.1.2.1 Pseudo-Header Fields
			// "Endpoints MUST treat a request or response
			// that contains undefined or invalid
			// pseudo-header fields as malformed (Section
			// 8.1.2.6)."
			sc.logf("invalid pseudo-header %q", f.Name)
			sc.req.invalidHeader = true
			return
		}
		if *dst != "" {
			sc.logf("duplicate pseudo-header %q sent", f.Name)
			sc.req.invalidHeader = true
			return
		}
		*dst = f.Value
	case f.Name == "cookie":
		sc.req.sawRegularHeader = true
		if s, ok := sc.req.header["Cookie"]; ok && len(s) == 1 {
			s[0] = s[0] + "; " + f.Value
		} else {
			sc.req.header.Add("Cookie", f.Value)
		}
	default:
		sc.req.sawRegularHeader = true
		sc.req.header.Add(sc.canonicalHeader(f.Name), f.Value)
	}
}

func (sc *serverConn) canonicalHeader(v string) string {
	sc.serveG.check()
	// TODO: use a sync.Pool instead of putting the cache on *serverConn?
	cv, ok := sc.canonHeader[v]
	if !ok {
		cv = http.CanonicalHeaderKey(v)
		sc.canonHeader[v] = cv
	}
	return cv
}

// readFrames is the loop that reads incoming frames.
// It's run on its own goroutine.
func (sc *serverConn) readFrames() {
	processed := make(chan struct{}, 1)
	for {
		f, err := sc.framer.ReadFrame()
		if err != nil {
			close(sc.readFrameCh)
			sc.readFrameErrCh <- err
			return
		}
		sc.readFrameCh <- frameAndProcessed{f, processed}
		<-processed
	}
}

func (sc *serverConn) serve() {
	sc.serveG.check()
	defer sc.conn.Close()
	defer close(sc.doneServing)

	sc.vlogf("HTTP/2 connection from %v on %p", sc.conn.RemoteAddr(), sc.hs)

	// Read the client preface
	buf := make([]byte, len(ClientPreface))
	// TODO: timeout reading from the client
	if _, err := io.ReadFull(sc.conn, buf); err != nil {
		sc.logf("error reading client preface: %v", err)
		return
	}
	if !bytes.Equal(buf, clientPreface) {
		sc.logf("bogus greeting from client: %q", buf)
		return
	}
	sc.vlogf("client %v said hello", sc.conn.RemoteAddr())

	f, err := sc.framer.ReadFrame()
	if err != nil {
		sc.logf("error reading initial frame from client: %v", err)
		return
	}
	sf, ok := f.(*SettingsFrame)
	if !ok {
		sc.logf("invalid initial frame type %T received from client", f)
		return
	}
	if err := sf.ForeachSetting(sc.processSetting); err != nil {
		sc.logf("initial settings error: %v", err)
		return
	}

	// TODO: don't send two network packets for our SETTINGS + our
	// ACK of their settings.  But if we make framer write to a
	// *bufio.Writer, that increases the per-connection memory
	// overhead, and there could be many idle conns. So maybe some
	// liveswitchWriter-like thing where we only switch to a
	// *bufio Writer when we really need one temporarily, else go
	// back to an unbuffered writes by default.
	if err := sc.framer.WriteSettings( /* TODO: actual settings */ ); err != nil {
		sc.logf("error writing server's initial settings: %v", err)
		return
	}
	if err := sc.framer.WriteSettingsAck(); err != nil {
		sc.logf("error writing server's ack of client's settings: %v", err)
		return
	}

	go sc.readFrames()

	for {
		select {
		case hr := <-sc.writeHeaderCh:
			if err := sc.writeHeaderInLoop(hr); err != nil {
				sc.condlogf(err, "error writing response header: %v", err)
				return
			}
		case wu := <-sc.windowUpdateCh:
			if err := sc.sendWindowUpdateInLoop(wu); err != nil {
				sc.condlogf(err, "error writing window update: %v", err)
				return
			}
		case fp, ok := <-sc.readFrameCh:
			if !ok {
				err := <-sc.readFrameErrCh
				if err != io.EOF {
					errstr := err.Error()
					if !strings.Contains(errstr, "use of closed network connection") {
						sc.logf("client %s stopped sending frames: %v", sc.conn.RemoteAddr(), errstr)
					}
				}
				return
			}
			f := fp.f
			sc.vlogf("got %v: %#v", f.Header(), f)
			err := sc.processFrame(f)
			fp.processed <- struct{}{} // let readFrames proceed
			switch ev := err.(type) {
			case nil:
				// nothing.
			case StreamError:
				if err := sc.resetStreamInLoop(ev); err != nil {
					sc.logf("Error writing RSTSTream: %v", err)
					return
				}
			case ConnectionError:
				sc.logf("Disconnecting; %v", ev)
				return
			case goAwayFlowError:
				if err := sc.goAway(ErrCodeFlowControl); err != nil {
					sc.condlogf(err, "failed to GOAWAY: %v", err)
					return
				}
			default:
				sc.logf("Disconnection due to other error: %v", err)
				return
			}
		}
	}
}

func (sc *serverConn) goAway(code ErrCode) error {
	sc.serveG.check()
	sc.sentGoAway = true
	return sc.framer.WriteGoAway(sc.maxStreamID, code, nil)
}

func (sc *serverConn) resetStreamInLoop(se StreamError) error {
	sc.serveG.check()
	if err := sc.framer.WriteRSTStream(se.streamID, uint32(se.code)); err != nil {
		return err
	}
	delete(sc.streams, se.streamID)
	return nil
}

func (sc *serverConn) curHeaderStreamID() uint32 {
	sc.serveG.check()
	st := sc.req.stream
	if st == nil {
		return 0
	}
	return st.id
}

func (sc *serverConn) processFrame(f Frame) error {
	sc.serveG.check()

	if s := sc.curHeaderStreamID(); s != 0 {
		if cf, ok := f.(*ContinuationFrame); !ok {
			return ConnectionError(ErrCodeProtocol)
		} else if cf.Header().StreamID != s {
			return ConnectionError(ErrCodeProtocol)
		}
	}

	switch f := f.(type) {
	case *SettingsFrame:
		return sc.processSettings(f)
	case *HeadersFrame:
		return sc.processHeaders(f)
	case *ContinuationFrame:
		return sc.processContinuation(f)
	case *WindowUpdateFrame:
		return sc.processWindowUpdate(f)
	case *PingFrame:
		return sc.processPing(f)
	case *DataFrame:
		return sc.processData(f)
	default:
		log.Printf("Ignoring unknown frame %#v", f)
		return nil
	}
}

func (sc *serverConn) processPing(f *PingFrame) error {
	sc.serveG.check()
	if f.Flags.Has(FlagSettingsAck) {
		// 6.7 PING: " An endpoint MUST NOT respond to PING frames
		// containing this flag."
		return nil
	}
	if f.StreamID != 0 {
		// "PING frames are not associated with any individual
		// stream. If a PING frame is received with a stream
		// identifier field value other than 0x0, the recipient MUST
		// respond with a connection error (Section 5.4.1) of type
		// PROTOCOL_ERROR."
		return ConnectionError(ErrCodeProtocol)
	}
	return sc.framer.WritePing(true, f.Data)
}

func (sc *serverConn) processWindowUpdate(f *WindowUpdateFrame) error {
	sc.serveG.check()
	switch {
	case f.StreamID != 0: // stream-level flow control
		st := sc.streams[f.StreamID]
		if st == nil {
			// "WINDOW_UPDATE can be sent by a peer that has sent a
			// frame bearing the END_STREAM flag. This means that a
			// receiver could receive a WINDOW_UPDATE frame on a "half
			// closed (remote)" or "closed" stream. A receiver MUST
			// NOT treat this as an error, see Section 5.1."
			return nil
		}
		if !st.flow.add(int32(f.Increment)) {
			return StreamError{f.StreamID, ErrCodeFlowControl}
		}
	default: // connection-level flow control
		if !sc.flow.add(int32(f.Increment)) {
			return goAwayFlowError{}
		}
	}
	return nil
}

func (sc *serverConn) processSettings(f *SettingsFrame) error {
	sc.serveG.check()
	return f.ForeachSetting(sc.processSetting)
}

func (sc *serverConn) processSetting(s Setting) error {
	sc.serveG.check()
	sc.vlogf("processing setting %v", s)
	switch s.ID {
	case SettingInitialWindowSize:
		return sc.processSettingInitialWindowSize(s.Val)
	}
	log.Printf("TODO: handle %v", s)
	return nil
}

func (sc *serverConn) processSettingInitialWindowSize(val uint32) error {
	sc.serveG.check()
	if val > (1<<31 - 1) {
		// 6.5.2 Defined SETTINGS Parameters
		// "Values above the maximum flow control window size of
		// 231-1 MUST be treated as a connection error (Section
		// 5.4.1) of type FLOW_CONTROL_ERROR."
		return ConnectionError(ErrCodeFlowControl)
	}

	// "A SETTINGS frame can alter the initial flow control window
	// size for all current streams. When the value of
	// SETTINGS_INITIAL_WINDOW_SIZE changes, a receiver MUST
	// adjust the size of all stream flow control windows that it
	// maintains by the difference between the new value and the
	// old value."
	old := sc.initialWindowSize
	sc.initialWindowSize = int32(val)
	growth := sc.initialWindowSize - old // may be negative
	for _, st := range sc.streams {
		if !st.flow.add(growth) {
			// 6.9.2 Initial Flow Control Window Size
			// "An endpoint MUST treat a change to
			// SETTINGS_INITIAL_WINDOW_SIZE that causes any flow
			// control window to exceed the maximum size as a
			// connection error (Section 5.4.1) of type
			// FLOW_CONTROL_ERROR."
			return ConnectionError(ErrCodeFlowControl)
		}
	}
	return nil
}

func (sc *serverConn) processData(f *DataFrame) error {
	sc.serveG.check()
	// "If a DATA frame is received whose stream is not in "open"
	// or "half closed (local)" state, the recipient MUST respond
	// with a stream error (Section 5.4.2) of type STREAM_CLOSED."
	id := f.Header().StreamID
	st, ok := sc.streams[id]
	if !ok || (st.state != stateOpen && st.state != stateHalfClosedLocal) {
		return StreamError{id, ErrCodeStreamClosed}
	}
	if st.body == nil {
		// Not expecting data.
		// TODO: which error code?
		return StreamError{id, ErrCodeStreamClosed}
	}
	data := f.Data()

	// Sender sending more than they'd declared?
	if st.declBodyBytes != -1 && st.bodyBytes+int64(len(data)) > st.declBodyBytes {
		st.body.Close(fmt.Errorf("Sender tried to send more than declared Content-Length of %d bytes", st.declBodyBytes))
		return StreamError{id, ErrCodeStreamClosed}
	}
	if len(data) > 0 {
		// TODO: verify they're allowed to write with the flow control
		// window we'd advertised to them.
		// TODO: verify n from Write
		if _, err := st.body.Write(data); err != nil {
			return StreamError{id, ErrCodeStreamClosed}
		}
		st.bodyBytes += int64(len(data))
	}
	if f.Header().Flags.Has(FlagDataEndStream) {
		if st.declBodyBytes != -1 && st.declBodyBytes != st.bodyBytes {
			st.body.Close(fmt.Errorf("Request declared a Content-Length of %d but only wrote %d bytes",
				st.declBodyBytes, st.bodyBytes))
		} else {
			st.body.Close(io.EOF)
		}
	}
	return nil
}

func (sc *serverConn) processHeaders(f *HeadersFrame) error {
	sc.serveG.check()
	id := f.Header().StreamID
	if sc.sentGoAway {
		// Ignore.
		return nil
	}
	// http://http2.github.io/http2-spec/#rfc.section.5.1.1
	if id%2 != 1 || id <= sc.maxStreamID || sc.req.stream != nil {
		// Streams initiated by a client MUST use odd-numbered
		// stream identifiers. [...] The identifier of a newly
		// established stream MUST be numerically greater than all
		// streams that the initiating endpoint has opened or
		// reserved. [...]  An endpoint that receives an unexpected
		// stream identifier MUST respond with a connection error
		// (Section 5.4.1) of type PROTOCOL_ERROR.
		return ConnectionError(ErrCodeProtocol)
	}
	if id > sc.maxStreamID {
		sc.maxStreamID = id
	}
	st := &stream{
		id:    id,
		state: stateOpen,
		flow:  newFlow(sc.initialWindowSize),
	}
	if f.Header().Flags.Has(FlagHeadersEndStream) {
		st.state = stateHalfClosedRemote
	}
	sc.streams[id] = st
	sc.req = requestParam{
		stream: st,
		header: make(http.Header),
	}
	return sc.processHeaderBlockFragment(st, f.HeaderBlockFragment(), f.HeadersEnded())
}

func (sc *serverConn) processContinuation(f *ContinuationFrame) error {
	sc.serveG.check()
	st := sc.streams[f.Header().StreamID]
	if st == nil || sc.curHeaderStreamID() != st.id {
		return ConnectionError(ErrCodeProtocol)
	}
	return sc.processHeaderBlockFragment(st, f.HeaderBlockFragment(), f.HeadersEnded())
}

func (sc *serverConn) processHeaderBlockFragment(st *stream, frag []byte, end bool) error {
	sc.serveG.check()
	if _, err := sc.hpackDecoder.Write(frag); err != nil {
		// TODO: convert to stream error I assume?
		return err
	}
	if !end {
		return nil
	}
	if err := sc.hpackDecoder.Close(); err != nil {
		// TODO: convert to stream error I assume?
		return err
	}
	rw, req, err := sc.newWriterAndRequest()
	sc.req = requestParam{}
	if err != nil {
		return err
	}
	st.body = req.Body.(*requestBody).pipe // may be nil
	st.declBodyBytes = req.ContentLength
	go sc.runHandler(rw, req)
	return nil
}

func (sc *serverConn) newWriterAndRequest() (*responseWriter, *http.Request, error) {
	sc.serveG.check()
	rp := &sc.req
	if rp.invalidHeader || rp.method == "" || rp.path == "" ||
		(rp.scheme != "https" && rp.scheme != "http") {
		// See 8.1.2.6 Malformed Requests and Responses:
		//
		// Malformed requests or responses that are detected
		// MUST be treated as a stream error (Section 5.4.2)
		// of type PROTOCOL_ERROR."
		//
		// 8.1.2.3 Request Pseudo-Header Fields
		// "All HTTP/2 requests MUST include exactly one valid
		// value for the :method, :scheme, and :path
		// pseudo-header fields"
		return nil, nil, StreamError{rp.stream.id, ErrCodeProtocol}
	}
	var tlsState *tls.ConnectionState // make this non-nil if https
	if rp.scheme == "https" {
		// TODO: get from sc's ConnectionState
		tlsState = &tls.ConnectionState{}
	}
	authority := rp.authority
	if authority == "" {
		authority = rp.header.Get("Host")
	}
	bodyOpen := rp.stream.state == stateOpen
	body := &requestBody{
		sc:       sc,
		streamID: rp.stream.id,
	}
	req := &http.Request{
		Method:     rp.method,
		URL:        &url.URL{},
		RemoteAddr: sc.conn.RemoteAddr().String(),
		Header:     rp.header,
		RequestURI: rp.path,
		Proto:      "HTTP/2.0",
		ProtoMajor: 2,
		ProtoMinor: 0,
		TLS:        tlsState,
		Host:       authority,
		Body:       body,
	}
	if bodyOpen {
		body.pipe = &pipe{
			b: buffer{buf: make([]byte, 65536)}, // TODO: share/remove
		}
		body.pipe.c.L = &body.pipe.m

		if vv, ok := rp.header["Content-Length"]; ok {
			req.ContentLength, _ = strconv.ParseInt(vv[0], 10, 64)
		} else {
			req.ContentLength = -1
		}
	}
	rw := &responseWriter{
		sc:       sc,
		streamID: rp.stream.id,
		req:      req,
		body:     body,
	}
	return rw, req, nil
}

// Run on its own goroutine.
func (sc *serverConn) runHandler(rw *responseWriter, req *http.Request) {
	defer rw.handlerDone()
	// TODO: catch panics like net/http.Server
	sc.handler.ServeHTTP(rw, req)
}

// called from handler goroutines
func (sc *serverConn) writeData(streamID uint32, p []byte) (n int, err error) {
	// TODO: implement
	log.Printf("WRITE on %d: %q", streamID, p)
	return len(p), nil
}

// headerWriteReq is a request to write an HTTP response header from a server Handler.
type headerWriteReq struct {
	streamID    uint32
	httpResCode int
	h           http.Header // may be nil
	endStream   bool
}

// called from handler goroutines.
// h may be nil.
func (sc *serverConn) writeHeader(req headerWriteReq) {
	sc.writeHeaderCh <- req
}

func (sc *serverConn) writeHeaderInLoop(req headerWriteReq) error {
	sc.serveG.check()
	sc.headerWriteBuf.Reset()
	sc.hpackEncoder.WriteField(hpack.HeaderField{Name: ":status", Value: httpCodeString(req.httpResCode)})
	for k, vv := range req.h {
		for _, v := range vv {
			// TODO: for gargage, cache lowercase copies of headers at
			// least for common ones and/or popular recent ones for
			// this serverConn. LRU?
			sc.hpackEncoder.WriteField(hpack.HeaderField{Name: strings.ToLower(k), Value: v})
		}
	}
	headerBlock := sc.headerWriteBuf.Bytes()
	if len(headerBlock) > int(sc.maxWriteFrameSize) {
		// we'll need continuation ones.
		panic("TODO")
	}
	return sc.framer.WriteHeaders(HeadersFrameParam{
		StreamID:      req.streamID,
		BlockFragment: headerBlock,
		EndStream:     req.endStream,
		EndHeaders:    true, // no continuation yet
	})
}

type windowUpdateReq struct {
	streamID uint32
	n        uint32
}

// called from handler goroutines
func (sc *serverConn) sendWindowUpdate(streamID uint32, n int) {
	const maxUint32 = 2147483647
	for n >= maxUint32 {
		sc.windowUpdateCh <- windowUpdateReq{streamID, maxUint32}
		n -= maxUint32
	}
	if n > 0 {
		sc.windowUpdateCh <- windowUpdateReq{streamID, uint32(n)}
	}
}

func (sc *serverConn) sendWindowUpdateInLoop(wu windowUpdateReq) error {
	sc.serveG.check()
	// TODO: sc.bufferedOutput.StartBuffering()
	if err := sc.framer.WriteWindowUpdate(0, wu.n); err != nil {
		return err
	}
	if err := sc.framer.WriteWindowUpdate(wu.streamID, wu.n); err != nil {
		return err
	}
	// TODO: return sc.bufferedOutput.Flush()
	return nil
}

type requestBody struct {
	sc       *serverConn
	streamID uint32
	closed   bool
	pipe     *pipe // non-nil if we have a HTTP entity message body
}

var errClosedBody = errors.New("body closed by handler")

func (b *requestBody) Close() error {
	if b.pipe != nil {
		b.pipe.Close(errClosedBody)
	}
	b.closed = true
	return nil
}

func (b *requestBody) Read(p []byte) (n int, err error) {
	if b.pipe == nil {
		return 0, io.EOF
	}
	n, err = b.pipe.Read(p)
	if n > 0 {
		b.sc.sendWindowUpdate(b.streamID, n)
		// TODO: tell b.sc to send back 'n' flow control quota credits to the sender
	}
	return
}

type responseWriter struct {
	sc           *serverConn
	streamID     uint32
	wroteHeaders bool
	h            http.Header

	req  *http.Request
	body *requestBody // to close at end of request, if DATA frames didn't
}

// TODO: bufio writing of responseWriter. add Flush, add pools of
// bufio.Writers, adjust bufio writer sized based on frame size
// updates from peer? For now: naive.

func (w *responseWriter) Header() http.Header {
	if w.h == nil {
		w.h = make(http.Header)
	}
	return w.h
}

func (w *responseWriter) WriteHeader(code int) {
	if w.wroteHeaders {
		return
	}
	// TODO: defer actually writing this frame until a Flush or
	// handlerDone, like net/http's Server. then we can coalesce
	// e.g. a 204 response to have a Header response frame with
	// END_STREAM set, without a separate frame being sent in
	// handleDone.
	w.wroteHeaders = true
	w.sc.writeHeader(headerWriteReq{
		streamID:    w.streamID,
		httpResCode: code,
		h:           w.h,
	})
}

// TODO: responseWriter.WriteString too?

func (w *responseWriter) Write(p []byte) (n int, err error) {
	if !w.wroteHeaders {
		w.WriteHeader(200)
	}
	return w.sc.writeData(w.streamID, p) // blocks waiting for tokens
}

func (w *responseWriter) handlerDone() {
	if !w.wroteHeaders {
		w.sc.writeHeader(headerWriteReq{
			streamID:    w.streamID,
			httpResCode: 200,
			h:           w.h,
			endStream:   true, // handler has finished; can't be any data.
		})
	}
}