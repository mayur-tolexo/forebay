package dataserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/mayur-tolexo/forebay/driver"
)

// DefaultIdle is how long a connection may hold a goroutine without asking
// anything, or without taking the answer.
//
// The far side is an NFS server on the same node, so this is not a defence
// against an attacker. It is a defence against the ordinary case of one
// hanging or being killed without closing, since a connection that never
// speaks again is indistinguishable from one that is about to.
const DefaultIdle = 5 * time.Minute

// DefaultExchangeBudget is how long serving one request and delivering its
// answer may take.
//
// Separate from the idle bound because they answer different questions: how
// long a caller may stay quiet, and how long an exchange may take. One number
// doing both jobs does neither, since the wait spends the budget the work
// then needs.
const DefaultExchangeBudget = 60 * time.Second

// maxConnections bounds how many conversations run at once. One NFS server
// needs a handful; a number far above that means something is wrong, and it
// is better to refuse than to keep accepting until the process cannot.
const maxConnections = 64

// Serve answers read requests on l until ctx is done.
//
// One connection is one conversation: a request, a reply, repeat. The far
// side is an NFS server on the same node, so there is no authentication here
// and the socket's own permissions are the boundary. Anything reachable over
// a network needs more than this.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	var wg sync.WaitGroup
	defer wg.Wait()

	// Cancelled before the wait above, since defers unwind in reverse: a
	// worker that stopped only on the caller's context would outlive a Serve
	// that returned because its listener broke, and the wait would never end.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Closing the listener is what unblocks Accept, since Accept does not
	// take a context. The watcher stops with Serve so it does not outlive it.
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
		case <-stopped:
		}
		l.Close()
	}()

	// Started with Serve and stopped with it, so predictions are fetched only
	// while there is somebody to fetch them for. A server used directly,
	// without Serve, still predicts and drops what it predicts, which is the
	// same cost as not having predicted.
	if s.ahead != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.fetchAhead(ctx)
		}()
	}

	// A token per conversation, taken before one is started rather than
	// after, so a refusal is a closed connection instead of a goroutine that
	// exists to say no.
	slots := make(chan struct{}, maxConnections)

	for {
		conn, err := l.Accept()
		if err != nil {
			// A listener closed on the way out is the ordinary end, not a
			// failure to report.
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("dataserver: accepting: %w", err)
		}
		select {
		case slots <- struct{}{}:
		default:
			// Closed rather than queued: the far side is one NFS server, so
			// this many at once is a fault to make visible, not load to
			// absorb.
			conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer conn.Close()
			// Closing the connection is what unblocks a conversation waiting
			// on a request, since a read does not take a context. Without it
			// a shutdown waits out the idle deadline on every open
			// connection, and an NFS server holding an idle one is the
			// ordinary state rather than an unusual one.
			finished := make(chan struct{})
			defer close(finished)
			go func() {
				select {
				case <-ctx.Done():
					conn.Close()
				case <-finished:
				}
			}()
			s.converse(ctx, conn)
		}()
	}
}

// converse answers requests on one connection until it ends or the read is
// something this cannot make sense of.
//
// A malformed frame closes the connection rather than being answered. The
// stream's framing is already lost by then, so a reply would land in the
// middle of whatever the far side thinks it is reading.
func (s *Server) converse(ctx context.Context, conn net.Conn) {
	for {
		// Waiting for a request is bounded on its own, because a caller that
		// has stopped speaking is indistinguishable from one about to.
		if err := conn.SetReadDeadline(time.Now().Add(s.idle)); err != nil {
			return
		}
		req, err := decodeRequest(conn)
		if err != nil {
			return
		}

		// Serving and answering get a budget of their own, starting now.
		// Sharing one clock with the wait meant whatever the caller spent
		// being quiet came out of the time to answer it, so a connection idle
		// for most of the bound had the remainder to do the work in, and the
		// first read after a quiet spell is the one most likely to miss and
		// go to the backend. Both directions, since a reply nobody takes
		// blocks the write with nothing to end it.
		if err := conn.SetDeadline(time.Now().Add(s.exchange)); err != nil {
			return
		}
		data, err := s.ReadRange(ctx, req.Tenant, req.Object, req.Offset, req.Length)
		if err := encodeReply(conn, statusFor(err), data); err != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// statusFor maps a read's outcome onto the wire.
//
// The distinction the data server draws between a bad range and a backend
// that could not answer has to survive the crossing, because the NFS server
// in front of it owes a client different errors for the two and cannot tell
// them apart from a failed read alone.
func statusFor(err error) Status {
	switch {
	case err == nil:
		return StatusOK
	case errors.Is(err, driver.ErrRange):
		return StatusRange
	case errors.Is(err, ErrRefused):
		return StatusRefused
	}
	// Anything not recognised is a failure, because claiming a request was
	// malformed when the backend was simply down sends somebody to fix the
	// wrong thing, while the reverse only costs a retry.
	return StatusFailed
}
