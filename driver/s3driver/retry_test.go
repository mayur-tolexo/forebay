package s3driver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flaky answers with status for the first n requests, then serves body.
type flaky struct {
	status int
	n      int32
	seen   atomic.Int32
	body   []byte
}

func (f *flaky) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.seen.Add(1) <= f.n {
		w.WriteHeader(f.status)
		return
	}
	w.Header().Set("Content-Length", "4")
	w.WriteHeader(http.StatusPartialContent)
	w.Write(f.body[:4])
}

// newFlaky wires a driver with no backoff, so a test measures the retrying
// rather than the waiting.
func newFlaky(t *testing.T, h http.Handler, attempts int) *Driver {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	d, err := New(Config{
		Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s",
		HTTPClient: srv.Client(), Attempts: attempts,
	})
	if err != nil {
		t.Fatal(err)
	}
	d.backoff = 0
	return d
}

// TestTransientFailureIsRetried covers the gap this exists for: nothing above
// the driver retries, so a 503 would reach an NFS client as an IO error.
func TestTransientFailureIsRetried(t *testing.T) {
	f := &flaky{status: http.StatusServiceUnavailable, n: 2, body: []byte("ABCD")}
	d := newFlaky(t, f, 3)

	got, err := d.ReadRange(context.Background(), "o", 0, 4)
	if err != nil {
		t.Fatalf("read = %v, want it to survive two 503s", err)
	}
	if string(got) != "ABCD" {
		t.Errorf("read %q, want ABCD", got)
	}
	if n := f.seen.Load(); n != 3 {
		t.Errorf("the store saw %d requests, want 3", n)
	}
}

// TestRetriesAreBounded keeps a store that is down from holding a read open.
func TestRetriesAreBounded(t *testing.T) {
	f := &flaky{status: http.StatusInternalServerError, n: 100}
	d := newFlaky(t, f, 3)

	_, err := d.ReadRange(context.Background(), "o", 0, 4)
	if err == nil {
		t.Fatal("a store that never answers returned no error")
	}
	if !strings.Contains(err.Error(), "after 3 attempts") {
		t.Errorf("err = %v, want it to say how many attempts were made", err)
	}
	if n := f.seen.Load(); n != 3 {
		t.Errorf("the store saw %d requests, want 3", n)
	}
}

// TestRefusalsAreNotRetried keeps an answer from being asked for again. A
// missing object is the store's answer, not a failure to give one.
func TestRefusalsAreNotRetried(t *testing.T) {
	var seen atomic.Int32
	d := newFlaky(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}), 3)

	if _, err := d.ReadRange(context.Background(), "o", 0, 4); err == nil {
		t.Fatal("a missing object returned no error")
	}
	if n := seen.Load(); n != 1 {
		t.Errorf("a 404 was sent %d times, want 1", n)
	}
}

// TestRetriedWriteResendsItsBody covers the request being rebuilt rather than
// reused: a body is consumed by the attempt that failed, and the payload hash
// is signed over it.
func TestRetriedWriteResendsItsBody(t *testing.T) {
	var seen atomic.Int32
	body := []byte("written twice")
	d := newFlaky(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("second attempt carried no body: %v", err)
		}
		if string(got) != string(body) {
			t.Errorf("second attempt sent %q, want %q", got, body)
		}
		if h := r.Header.Get("x-amz-content-sha256"); h != sha256Hex(got) {
			t.Errorf("payload hash does not match the resent body")
		}
		w.WriteHeader(http.StatusOK)
	}), 3)

	if err := d.WriteObject(context.Background(), "o", body); err != nil {
		t.Fatalf("write = %v", err)
	}
}

// TestGivingUpStopsTheRetries keeps a cancelled caller from waiting out the
// remaining attempts.
func TestGivingUpStopsTheRetries(t *testing.T) {
	f := &flaky{status: http.StatusServiceUnavailable, n: 100}
	srv := httptest.NewServer(f)
	defer srv.Close()
	d, err := New(Config{
		Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s",
		HTTPClient: srv.Client(), Attempts: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	d.backoff = 50 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := d.ReadRange(ctx, "o", 0, 4); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the caller's deadline", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %v, want it to stop when the caller did", took)
	}
}
