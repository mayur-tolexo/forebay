package grpcwire

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Client calls a gRPC server over a Unix socket.
//
// It exists so this project can exercise its own driver end to end, and so an
// operator can ask a running driver a question without a gRPC toolchain on the
// node.
type Client struct {
	http *http.Client
	// authority fills the :authority pseudo-header. A Unix socket has no
	// host, and HTTP/2 requires one.
	authority string
}

// Dial points a client at a socket. Nothing is connected until the first call,
// so this cannot fail on a driver that has not started yet.
func Dial(network, address string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetUnencryptedHTTP2(true)
	return &Client{http: &http.Client{Transport: tr}, authority: "forebay"}
}

// Close releases idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// Call makes one unary call and returns the reply message.
func (c *Client) Call(ctx context.Context, fullMethod string, req []byte) ([]byte, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+c.authority+fullMethod, bytes.NewReader(frame(req)))
	if err != nil {
		return nil, Errorf(Internal, "building the request: %v", err)
	}
	r.Header.Set("Content-Type", "application/grpc+proto")
	// Announced, because a server is allowed to refuse to send trailers to a
	// caller that did not say it reads them, and the outcome is in a trailer.
	r.Header.Set("TE", "trailers")
	if d, ok := ctx.Deadline(); ok {
		if left := time.Until(d); left > 0 {
			r.Header.Set("grpc-timeout", strconv.FormatInt(int64(left/time.Microsecond), 10)+"u")
		}
	}

	resp, err := c.http.Do(r)
	if err != nil {
		return nil, Errorf(Unavailable, "calling %s: %v", fullMethod, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// A non-200 means the far side did not treat this as a gRPC call at
		// all, so there is no trailer to read the outcome from.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return nil, Errorf(Unavailable, "%s answered HTTP %d: %s", fullMethod, resp.StatusCode, bytes.TrimSpace(body))
	}

	msg, readErr := readMessage(resp.Body)
	// Drained before the trailers are read, because the standard library only
	// fills them once the body is done: reading them any earlier reads
	// nothing and every call looks like it carried no outcome.
	io.Copy(io.Discard, resp.Body)

	if st := status(resp.Trailer); st != nil {
		// The status wins over a body that could not be read. A failed call
		// carries no message, so "no message" is the expected shape here
		// rather than a second problem to report.
		return nil, st
	}
	if readErr != nil {
		return nil, readErr
	}
	return msg, nil
}

// status reads the outcome out of the trailers, returning nil for a call that
// succeeded.
//
// A missing grpc-status is Internal rather than success: a reply with no
// outcome is a truncated one, and reading it as OK would turn a connection cut
// mid-call into an empty but valid answer.
func status(t http.Header) error {
	raw := t.Get("grpc-status")
	if raw == "" {
		return Errorf(Internal, "the reply carried no grpc-status")
	}
	code, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return Errorf(Internal, "unreadable grpc-status %q", raw)
	}
	if Code(code) == OK {
		return nil
	}
	return &Status{Code: Code(code), Message: decodeMessage(t.Get("grpc-message"))}
}
