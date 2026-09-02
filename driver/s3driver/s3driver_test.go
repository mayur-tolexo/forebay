package s3driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mayur-tolexo/forebay/driver"
	"github.com/mayur-tolexo/forebay/driver/conformance"
)

// fakeS3 answers the way a store does, including the two behaviours the driver
// exists to cope with: an offset past the end is a 416, and a length running
// off the end is a 206 that is simply shorter.
type fakeS3 struct {
	objects map[string][]byte
	// requests counts what was actually sent, so a test can tell a driver that
	// answered from one that answered without asking.
	requests int
}

func (f *fakeS3) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests++
		if r.Header.Get("Authorization") == "" {
			t.Errorf("unsigned %s %s", r.Method, r.URL.Path)
		}
		key := strings.TrimPrefix(r.URL.Path, "/bucket/")

		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("reading put body: %v", err)
			}
			// Against the body rather than merely present. A store verifies
			// this, so a driver that signs the empty hash over a real body is
			// rejected there and would pass a check that only asks if it is set.
			if got := r.Header.Get("x-amz-content-sha256"); got != sha256Hex(body) {
				t.Errorf("payload hash %s does not match the %d byte body", got, len(body))
			}
			f.objects[key] = body
			w.WriteHeader(http.StatusOK)
			return
		case http.MethodDelete:
			delete(f.objects, key)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if got := r.Header.Get("x-amz-content-sha256"); got != emptyPayload {
			t.Errorf("%s %s carried payload hash %q, want the empty one", r.Method, r.URL.Path, got)
		}

		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `<Error><Code>NoSuchKey</Code><Message>not here</Message></Error>`)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}

		first, last, ok := parseRange(r.Header.Get("Range"))
		if !ok {
			w.Write(data)
			return
		}
		if first >= len(data) {
			w.Header().Del("Content-Length")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if last >= len(data) {
			last = len(data) - 1
		}
		part := data[first : last+1]
		w.Header().Set("Content-Length", strconv.Itoa(len(part)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(part)
	})
}

// parseRange reads the one header form this driver sends.
func parseRange(h string) (int, int, bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	parts := strings.SplitN(strings.TrimPrefix(h, "bytes="), "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	first, err1 := strconv.Atoi(parts[0])
	last, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return first, last, true
}

// newFake wires a driver to a fake store holding one object.
func newFake(t *testing.T, objects map[string][]byte) (*Driver, *fakeS3) {
	t.Helper()
	fake := &fakeS3{objects: objects}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)

	d, err := New(Config{
		Endpoint:   srv.URL,
		Bucket:     "bucket",
		Region:     "us-east-1",
		AccessKey:  "key",
		SecretKey:  "secret",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, fake
}

func TestConformance(t *testing.T) {
	content := []byte("the quick brown fox jumps over the lazy dog")
	d, _ := newFake(t, map[string][]byte{"fixture": content})

	conformance.Run(t, conformance.Fixture{
		Driver:         d,
		Object:         "fixture",
		Content:        content,
		WritablePrefix: "scratch",
	})
}

// TestReadPastEndIsAnError covers the case a store answers with a short 206
// rather than a refusal, which a caller cannot tell from a truncated object.
func TestReadPastEndIsAnError(t *testing.T) {
	content := []byte("0123456789")
	d, _ := newFake(t, map[string][]byte{"o": content})

	if _, err := d.ReadRange(context.Background(), "o", 0, 11); !errors.Is(err, driver.ErrRange) {
		t.Errorf("reading 11 bytes of a 10 byte object = %v, want ErrRange", err)
	}
	if _, err := d.ReadRange(context.Background(), "o", 10, 1); !errors.Is(err, driver.ErrRange) {
		t.Errorf("reading past the end = %v, want ErrRange", err)
	}
}

// TestEmptyRangeStillChecksTheObject keeps a read of nothing from succeeding
// against a name that is not there.
func TestEmptyRangeStillChecksTheObject(t *testing.T) {
	d, fake := newFake(t, map[string][]byte{"o": []byte("abc")})
	ctx := context.Background()

	got, err := d.ReadRange(ctx, "o", 0, 0)
	if err != nil || len(got) != 0 {
		t.Errorf("empty range of a real object = %q, %v", got, err)
	}
	before := fake.requests
	if _, err := d.ReadRange(ctx, "missing", 0, 0); err == nil {
		t.Error("empty range of an absent object succeeded")
	}
	if fake.requests == before {
		t.Error("empty range answered without asking the store")
	}
}

// TestMissingObjectCarriesTheStoresReason checks the failure a operator reads.
func TestMissingObjectCarriesTheStoresReason(t *testing.T) {
	d, _ := newFake(t, map[string][]byte{})
	_, err := d.ReadRange(context.Background(), "gone", 0, 1)
	if err == nil || !strings.Contains(err.Error(), "NoSuchKey") {
		t.Errorf("error = %v, want the store's own code", err)
	}
}

// TestUndeclaredAreRefused checks the two this driver does not do answer with
// something a caller can tell from a transient failure.
func TestUndeclaredAreRefused(t *testing.T) {
	d, _ := newFake(t, map[string][]byte{})
	if _, err := d.SnapshotObject(context.Background(), "o"); !errors.Is(err, driver.ErrNotSupported) {
		t.Errorf("snapshot = %v, want ErrNotSupported", err)
	}
	if err := d.CloneObject(context.Background(), "a", "b"); !errors.Is(err, driver.ErrNotSupported) {
		t.Errorf("clone = %v, want ErrNotSupported", err)
	}
}

// TestConfigIsCheckedUpFront keeps a broken configuration from becoming a
// failed read much later, when the cause is no longer nearby.
func TestConfigIsCheckedUpFront(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  Config
	}{
		{"no endpoint", Config{Bucket: "b", AccessKey: "k", SecretKey: "s"}},
		{"endpoint without a scheme", Config{Endpoint: "example.com", Bucket: "b", AccessKey: "k", SecretKey: "s"}},
		{"no bucket", Config{Endpoint: "https://example.com", AccessKey: "k", SecretKey: "s"}},
		{"no credentials", Config{Endpoint: "https://example.com", Bucket: "b"}},
	} {
		if _, err := New(c.cfg); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

// TestBadObjectNamesAreRefused keeps a key that would address something else
// from being sent.
func TestBadObjectNamesAreRefused(t *testing.T) {
	d, fake := newFake(t, map[string][]byte{})
	for _, name := range []string{"", "/leading"} {
		if _, err := d.SizeOf(context.Background(), name); !errors.Is(err, ErrBadObject) {
			t.Errorf("SizeOf(%q) = %v, want ErrBadObject", name, err)
		}
	}
	if fake.requests != 0 {
		t.Errorf("a refused name still reached the store %d times", fake.requests)
	}
}
