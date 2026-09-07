package grpcwire

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxMessage bounds a single request body.
//
// It is gRPC's own default. The bound matters more here than there: a CSI
// socket is reachable by anything that can open it, and a length prefix is a
// caller's word for how much this side should allocate.
const maxMessage = 4 << 20

// Handler serves one unary method. The bytes in and out are encoded messages,
// which keeps this package free of any particular schema.
type Handler func(ctx context.Context, req []byte) ([]byte, error)

// Server routes gRPC calls to handlers.
type Server struct {
	mu       sync.RWMutex
	methods  map[string]Handler
	http     *http.Server
	onceInit sync.Once
}

// NewServer builds an empty one.
func NewServer() *Server {
	return &Server{methods: map[string]Handler{}}
}

// Register adds a method, named the way it travels: /package.Service/Method.
//
// It panics on a duplicate or a malformed name. Both are wiring mistakes in
// this binary rather than anything a peer can cause, and a driver that
// silently served the wrong handler for a method would be found much later.
func (s *Server) Register(fullMethod string, h Handler) {
	if !strings.HasPrefix(fullMethod, "/") || strings.Count(fullMethod, "/") != 2 {
		panic("grpcwire: a method is /package.Service/Method, got " + fullMethod)
	}
	if h == nil {
		panic("grpcwire: nil handler for " + fullMethod)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.methods[fullMethod]; taken {
		panic("grpcwire: " + fullMethod + " is registered twice")
	}
	s.methods[fullMethod] = h
}

// Serve answers calls on l until the listener is closed.
func (s *Server) Serve(l net.Listener) error {
	s.init()
	err := s.http.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops serving, letting calls in flight finish.
func (s *Server) Shutdown(ctx context.Context) error {
	s.init()
	return s.http.Shutdown(ctx)
}

// init builds the HTTP server once, whichever entry point is called first.
func (s *Server) init() {
	s.onceInit.Do(func() {
		h := &http.Server{Handler: http.HandlerFunc(s.serveHTTP)}
		// gRPC over a Unix socket is HTTP/2 with no TLS, which the standard
		// library will only do when it is asked to.
		h.Protocols = new(http.Protocols)
		h.Protocols.SetUnencryptedHTTP2(true)
		s.http = h
	})
}

// serveHTTP turns one HTTP/2 request into one gRPC call.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// A gRPC response says OK in the HTTP status whatever the call did, and
	// puts the outcome in a trailer. Declaring the trailers up front is what
	// makes the standard library send them.
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Trailer", "grpc-status, grpc-message")

	if r.Method != http.MethodPost {
		// Not a gRPC call at all. This is the one case answered with an HTTP
		// status, because a caller that did not speak gRPC will not read a
		// trailer.
		http.Error(w, "grpc requires POST", http.StatusMethodNotAllowed)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/grpc") {
		http.Error(w, "not a grpc content type: "+ct, http.StatusUnsupportedMediaType)
		return
	}

	s.mu.RLock()
	h, known := s.methods[r.URL.Path]
	s.mu.RUnlock()
	if !known {
		// Unimplemented rather than a 404: a sidecar probes for optional
		// methods and reads this code as "not offered".
		finish(w, Errorf(Unimplemented, "%s is not served by this driver", r.URL.Path))
		return
	}

	ctx := r.Context()
	if d, ok := timeout(r.Header.Get("grpc-timeout")); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	req, err := readMessage(r.Body)
	if err != nil {
		finish(w, err)
		return
	}

	resp, err := h(ctx, req)
	if err != nil {
		// No body on a failure. A caller reads the trailer first and a
		// half-written message under a non-zero status is a second thing for
		// it to make sense of.
		finish(w, err)
		return
	}
	if _, err := w.Write(frame(resp)); err != nil {
		// The body is already going out, so the status cannot be changed.
		// The trailer still gets set, which is what a caller reads.
		finish(w, Errorf(Internal, "writing the reply: %v", err))
		return
	}
	finish(w, nil)
}

// finish writes the outcome into the trailers.
func finish(w http.ResponseWriter, err error) {
	code := CodeOf(err)
	w.Header().Set("grpc-status", strconv.FormatUint(uint64(code), 10))
	if err != nil {
		var s *Status
		if errors.As(err, &s) {
			w.Header().Set("grpc-message", encodeMessage(s.Message))
			return
		}
		w.Header().Set("grpc-message", encodeMessage(err.Error()))
		return
	}
	w.Header().Set("grpc-message", "")
}

// frame puts gRPC's five-byte prefix on a message: one byte saying it is not
// compressed, then the length.
func frame(msg []byte) []byte {
	out := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(msg)))
	copy(out[5:], msg)
	return out
}

// readMessage reads one framed message.
//
// Exactly one: this serves unary calls, and a second message on the stream is
// a caller using a streaming method against a driver that has none.
func readMessage(r io.Reader) ([]byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, Errorf(InvalidArgument, "the request carried no message")
		}
		return nil, Errorf(InvalidArgument, "reading the message header: %v", err)
	}
	if head[0] != 0 {
		// The flag names a compressor, and one is only ever set after the
		// call agreed on it in a header this does not send.
		return nil, Errorf(Unimplemented, "compressed messages are not accepted")
	}
	n := binary.BigEndian.Uint32(head[1:5])
	if n > maxMessage {
		return nil, Errorf(ResourceExhausted, "a message of %d bytes, over the %d limit", n, maxMessage)
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, Errorf(InvalidArgument, "the message ended after fewer than the %d bytes it declared", n)
	}
	return msg, nil
}

// timeout reads gRPC's own header, which is a number and a unit letter.
//
// An unreadable value is no deadline rather than an error: the call can still
// be served, and refusing it would turn a caller's malformed hint into a
// failure of the thing it was asking for.
func timeout(v string) (time.Duration, bool) {
	if len(v) < 2 {
		return 0, false
	}
	n, err := strconv.ParseInt(v[:len(v)-1], 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	var unit time.Duration
	switch v[len(v)-1] {
	case 'H':
		unit = time.Hour
	case 'M':
		unit = time.Minute
	case 'S':
		unit = time.Second
	case 'm':
		unit = time.Millisecond
	case 'u':
		unit = time.Microsecond
	case 'n':
		unit = time.Nanosecond
	default:
		return 0, false
	}
	return time.Duration(n) * unit, true
}
