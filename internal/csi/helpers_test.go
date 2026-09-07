package csi_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/grpcwire"
	"github.com/mayur-tolexo/forebay/internal/protowire"
)

// callServer runs one call against a real server over a real socket.
//
// The handlers could be called directly, but then nothing would exercise the
// encoding an orchestrator actually sends, which is where a field number is
// wrong in a way no round-trip inside this package would show.
func callServer(t *testing.T, s *grpcwire.Server, method string, req []byte) ([]byte, error) {
	t.Helper()
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
	return c.Call(t.Context(), method, req)
}

// decodeVolume reads a CreateVolumeResponse.
func decodeVolume(t *testing.T, b []byte) (id string, bytes int64, ctx map[string]string) {
	t.Helper()
	ctx = map[string]string{}
	for len(b) > 0 {
		f, rest, err := protowire.Next(b)
		if err != nil {
			t.Fatalf("decoding the reply: %v", err)
		}
		if f.Number == 1 {
			inner := f.Bytes
			for len(inner) > 0 {
				g, more, err := protowire.Next(inner)
				if err != nil {
					t.Fatalf("decoding the volume: %v", err)
				}
				switch g.Number {
				case 1:
					bytes = g.Int64()
				case 2:
					id = g.Text()
				case 3:
					if err := protowire.StringMap(ctx, g.Bytes); err != nil {
						t.Fatalf("decoding the context: %v", err)
					}
				}
				inner = more
			}
		}
		b = rest
	}
	return id, bytes, ctx
}

// decodeValidate reads a ValidateVolumeCapabilitiesResponse.
//
// A confirmation is the message being present rather than a boolean in it,
// which is what the specification says and what a caller reads.
func decodeValidate(t *testing.T, b []byte) (confirmed bool, why string) {
	t.Helper()
	for len(b) > 0 {
		f, rest, err := protowire.Next(b)
		if err != nil {
			t.Fatalf("decoding the reply: %v", err)
		}
		switch f.Number {
		case 1:
			confirmed = true
		case 2:
			why = f.Text()
		}
		b = rest
	}
	return confirmed, why
}

// decodeName reads a GetPluginInfoResponse.
func decodeName(t *testing.T, b []byte) string {
	t.Helper()
	var name string
	for len(b) > 0 {
		f, rest, err := protowire.Next(b)
		if err != nil {
			t.Fatalf("decoding the reply: %v", err)
		}
		if f.Number == 1 {
			name = f.Text()
		}
		b = rest
	}
	return name
}
