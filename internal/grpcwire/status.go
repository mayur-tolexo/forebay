// Package grpcwire carries gRPC calls over a Unix socket.
//
// CSI is a gRPC service and this project takes no dependencies, so the
// transport is here. It is the small half of gRPC: unary calls, no streaming,
// no compression, no interceptors, no reflection. That is all CSI uses.
//
// The heavy lifting is HTTP/2, and the standard library does it. gRPC is
// HTTP/2 with a five-byte length prefix on each message and the call's outcome
// in a trailer rather than in the response status, which is why the response
// code is 200 even for a call that failed.
package grpcwire

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Code is a gRPC status code.
//
// The numbers are the protocol's and cannot be renumbered: they travel on the
// wire as decimal in the grpc-status trailer, and the sidecars that call a CSI
// driver switch on them. Only the ones this project returns are named.
type Code uint32

const (
	// OK is a call that succeeded.
	OK Code = 0
	// Canceled is the caller giving up, which is what a context cancellation
	// becomes.
	Canceled Code = 1
	// Unknown is an error with no better code, which is what a handler
	// returning a plain error means.
	Unknown Code = 2
	// InvalidArgument is a request that no retry would fix.
	InvalidArgument Code = 3
	// DeadlineExceeded is the caller's own timeout, arriving as a header.
	DeadlineExceeded Code = 4
	// NotFound is a dataset that does not exist.
	NotFound Code = 5
	// ResourceExhausted is a request over a limit, which is what a message
	// larger than this transport accepts becomes.
	ResourceExhausted Code = 8
	// FailedPrecondition is a request that is well-formed and cannot be done
	// in the current state.
	FailedPrecondition Code = 9
	// Unimplemented is a method this driver does not serve. CSI sidecars
	// probe for optional methods and read this as "not offered" rather than
	// as a failure, so it has to be this code and not Unknown.
	Unimplemented Code = 12
	// Internal is a defect on this side.
	Internal Code = 13
	// Unavailable is a dependency that may be back shortly, which is the code
	// a caller is expected to retry.
	Unavailable Code = 14
)

// String names a code for a log line.
func (c Code) String() string {
	switch c {
	case OK:
		return "OK"
	case Canceled:
		return "Canceled"
	case Unknown:
		return "Unknown"
	case InvalidArgument:
		return "InvalidArgument"
	case ResourceExhausted:
		return "ResourceExhausted"
	case DeadlineExceeded:
		return "DeadlineExceeded"
	case NotFound:
		return "NotFound"
	case FailedPrecondition:
		return "FailedPrecondition"
	case Unimplemented:
		return "Unimplemented"
	case Internal:
		return "Internal"
	case Unavailable:
		return "Unavailable"
	default:
		return "Code(" + strconv.FormatUint(uint64(c), 10) + ")"
	}
}

// Status is a call's outcome, and is an error when the code is not OK.
type Status struct {
	Code    Code
	Message string
}

// Error renders a status the way a caller will print it.
func (s *Status) Error() string { return "rpc " + s.Code.String() + ": " + s.Message }

// Errorf builds a status error.
func Errorf(c Code, format string, args ...any) error {
	return &Status{Code: c, Message: fmt.Sprintf(format, args...)}
}

// CodeOf reports the code an error should travel as.
//
// An error that is not a Status is Unknown rather than Internal: Internal
// claims this side is at fault, and an error that never said what it was has
// not claimed anything.
func CodeOf(err error) Code {
	if err == nil {
		return OK
	}
	var s *Status
	if errors.As(err, &s) {
		return s.Code
	}
	return Unknown
}

// encodeMessage percent-encodes a status message for a trailer.
//
// A trailer holds only printable ASCII, and a message here can contain a path
// or an error from elsewhere with anything in it. The escaping is gRPC's own,
// so a caller that decodes it gets the message back.
func encodeMessage(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7E || c == '%' {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// decodeMessage reverses it, leaving anything malformed as it stands.
//
// A message is for a person to read, so a bad escape is shown rather than
// turned into an error that replaces the message that was being reported.
func decodeMessage(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
