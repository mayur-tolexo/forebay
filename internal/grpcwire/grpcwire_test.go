package grpcwire_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/grpcwire"
)

// serve starts a server on a socket in the test's own directory and returns a
// client pointed at it.
func serve(t *testing.T, register func(*grpcwire.Server)) *grpcwire.Client {
	t.Helper()
	// A Unix socket path is bounded at around a hundred bytes and t.TempDir
	// spells the test's name into the directory, which on its own goes past
	// that. This one is short on purpose.
	dir, err := os.MkdirTemp("", "fb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	s := grpcwire.NewServer()
	register(s)

	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()

	c := grpcwire.Dial("unix", sock)
	t.Cleanup(func() {
		c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Shutdown(ctx)
		if err := <-done; err != nil {
			t.Errorf("serving: %v", err)
		}
	})
	return c
}

func TestACallCarriesBytesBothWays(t *testing.T) {
	c := serve(t, func(s *grpcwire.Server) {
		s.Register("/csi.v1.Identity/Probe", func(ctx context.Context, req []byte) ([]byte, error) {
			return append([]byte("saw:"), req...), nil
		})
	})
	got, err := c.Call(t.Context(), "/csi.v1.Identity/Probe", []byte("a request"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "saw:a request" {
		t.Errorf("got %q", got)
	}
}

func TestAnEmptyMessageIsAMessage(t *testing.T) {
	// Most CSI requests are empty, and an empty body is not a missing one: a
	// transport that could not tell them apart would fail every Probe.
	c := serve(t, func(s *grpcwire.Server) {
		s.Register("/csi.v1.Identity/Probe", func(ctx context.Context, req []byte) ([]byte, error) {
			if len(req) != 0 {
				return nil, grpcwire.Errorf(grpcwire.Internal, "expected nothing, got %d bytes", len(req))
			}
			return nil, nil
		})
	})
	got, err := c.Call(t.Context(), "/csi.v1.Identity/Probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d bytes back", len(got))
	}
}

func TestAFailureArrivesWithItsCodeAndMessage(t *testing.T) {
	// A CSI sidecar switches on the code, so it has to survive the trip, and
	// an operator reads the message, so it has to as well.
	c := serve(t, func(s *grpcwire.Server) {
		s.Register("/csi.v1.Controller/CreateVolume", func(ctx context.Context, req []byte) ([]byte, error) {
			return nil, grpcwire.Errorf(grpcwire.NotFound, "no dataset named %q", "imagenet/v99")
		})
	})
	_, err := c.Call(t.Context(), "/csi.v1.Controller/CreateVolume", nil)
	if grpcwire.CodeOf(err) != grpcwire.NotFound {
		t.Fatalf("got code %v from %v", grpcwire.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), `no dataset named "imagenet/v99"`) {
		t.Errorf("message did not survive: %v", err)
	}
}

func TestAMessageWithBytesATrailerCannotHoldStillArrives(t *testing.T) {
	// A status message carries paths and errors from elsewhere, and a trailer
	// holds only printable ASCII. Without escaping, the header is either
	// rejected or silently mangled.
	awkward := "mounting /var/lib/kubelet: 100% full\nnewline\ttab and é"
	c := serve(t, func(s *grpcwire.Server) {
		s.Register("/csi.v1.Node/NodePublishVolume", func(ctx context.Context, req []byte) ([]byte, error) {
			return nil, grpcwire.Errorf(grpcwire.Internal, "%s", awkward)
		})
	})
	_, err := c.Call(t.Context(), "/csi.v1.Node/NodePublishVolume", nil)
	if err == nil {
		t.Fatal("the call succeeded")
	}
	if !strings.Contains(err.Error(), awkward) {
		t.Errorf("got %q,\nwant it to contain %q", err.Error(), awkward)
	}
}

func TestAMethodThisDriverDoesNotServeIsUnimplemented(t *testing.T) {
	// Sidecars probe for optional methods. Unimplemented means "not offered"
	// and anything else means "broken", so the difference decides whether a
	// sidecar carries on or backs off.
	c := serve(t, func(s *grpcwire.Server) {
		s.Register("/csi.v1.Identity/Probe", func(ctx context.Context, req []byte) ([]byte, error) {
			return nil, nil
		})
	})
	_, err := c.Call(t.Context(), "/csi.v1.Controller/CreateSnapshot", nil)
	if grpcwire.CodeOf(err) != grpcwire.Unimplemented {
		t.Errorf("got %v from %v", grpcwire.CodeOf(err), err)
	}
}

func TestAPlainErrorIsUnknownRatherThanInternal(t *testing.T) {
	// Internal claims this side is at fault. A handler returning an error it
	// never classified has claimed nothing, and saying Internal on its behalf
	// would send a caller looking for a defect here.
	c := serve(t, func(s *grpcwire.Server) {
		s.Register("/csi.v1.Identity/Probe", func(ctx context.Context, req []byte) ([]byte, error) {
			return nil, errors.New("something went wrong")
		})
	})
	_, err := c.Call(t.Context(), "/csi.v1.Identity/Probe", nil)
	if grpcwire.CodeOf(err) != grpcwire.Unknown {
		t.Errorf("got %v", grpcwire.CodeOf(err))
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Errorf("message lost: %v", err)
	}
}

func TestTheCallersDeadlineReachesTheHandler(t *testing.T) {
	// A sidecar sets a timeout and expects the driver to stop when it fires.
	// Without the header, a handler waiting on a mount would run on after the
	// caller had given up.
	saw := make(chan bool, 1)
	c := serve(t, func(s *grpcwire.Server) {
		s.Register("/csi.v1.Identity/Probe", func(ctx context.Context, req []byte) ([]byte, error) {
			_, ok := ctx.Deadline()
			saw <- ok
			return nil, nil
		})
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := c.Call(ctx, "/csi.v1.Identity/Probe", nil); err != nil {
		t.Fatal(err)
	}
	if !<-saw {
		t.Error("the handler ran with no deadline")
	}
}

func TestAMessageOverTheLimitIsRefusedWithoutAllocatingIt(t *testing.T) {
	// The length is the caller's word for how much this side should allocate,
	// and the socket is reachable by anything that can open it.
	c := serve(t, func(s *grpcwire.Server) {
		s.Register("/csi.v1.Identity/Probe", func(ctx context.Context, req []byte) ([]byte, error) {
			return nil, nil
		})
	})
	_, err := c.Call(t.Context(), "/csi.v1.Identity/Probe", make([]byte, (4<<20)+1))
	if grpcwire.CodeOf(err) != grpcwire.ResourceExhausted {
		t.Errorf("got %v from %v", grpcwire.CodeOf(err), err)
	}
}

func TestRegisteringTheSameMethodTwiceIsRefused(t *testing.T) {
	// Two handlers for one method is a wiring mistake in this binary, and the
	// one that answered would be whichever the map kept.
	defer func() {
		if recover() == nil {
			t.Error("a duplicate registration was accepted")
		}
	}()
	s := grpcwire.NewServer()
	h := func(ctx context.Context, req []byte) ([]byte, error) { return nil, nil }
	s.Register("/csi.v1.Identity/Probe", h)
	s.Register("/csi.v1.Identity/Probe", h)
}

func TestAMalformedMethodNameIsRefused(t *testing.T) {
	for _, name := range []string{"csi.v1.Identity/Probe", "/Probe", "/a/b/c", ""} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%q was accepted as a method name", name)
				}
			}()
			grpcwire.NewServer().Register(name, func(ctx context.Context, req []byte) ([]byte, error) {
				return nil, nil
			})
		}()
	}
}

func TestManyCallsOnOneConnection(t *testing.T) {
	// A sidecar holds one connection open for the life of the driver, so the
	// framing has to leave the stream exactly where the next call starts.
	c := serve(t, func(s *grpcwire.Server) {
		s.Register("/csi.v1.Identity/GetPluginInfo", func(ctx context.Context, req []byte) ([]byte, error) {
			return []byte(fmt.Sprintf("reply to %d bytes", len(req))), nil
		})
	})
	for i := 0; i < 50; i++ {
		got, err := c.Call(t.Context(), "/csi.v1.Identity/GetPluginInfo", make([]byte, i))
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if want := fmt.Sprintf("reply to %d bytes", i); string(got) != want {
			t.Fatalf("call %d got %q, want %q", i, got, want)
		}
	}
}

func TestAReplyWithNoOutcomeIsNotSuccess(t *testing.T) {
	// gRPC puts the outcome in a trailer, so a reply that carries a body and
	// no trailer is a call that was cut off partway. Reading it as success
	// turns a connection lost mid-call into an empty but valid answer, which
	// for GetPluginInfo is a driver with no name.
	dir, err := os.MkdirTemp("", "fb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}

	// Deliberately not grpcwire.Server: this is a peer that speaks HTTP/2 and
	// stops short of finishing the call.
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte{0, 0, 0, 0, 2, 0x08, 0x01})
	})}
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetUnencryptedHTTP2(true)
	go srv.Serve(l)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	c := grpcwire.Dial("unix", sock)
	t.Cleanup(c.Close)

	_, err = c.Call(t.Context(), "/csi.v1.Identity/GetPluginInfo", nil)
	if err == nil {
		t.Fatal("a reply with no grpc-status was read as success")
	}
	if grpcwire.CodeOf(err) != grpcwire.Internal {
		t.Errorf("got %v from %v", grpcwire.CodeOf(err), err)
	}
	// Named rather than lumped in with an unreadable one: a missing status is
	// a call that was cut off and a malformed one is a peer sending nonsense,
	// and the operator chasing them looks in two different places.
	if !strings.Contains(err.Error(), "carried no grpc-status") {
		t.Errorf("got %q, want it to say the status was missing", err)
	}
}
