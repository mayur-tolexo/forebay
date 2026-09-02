package bench

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeReader serves a byte range out of one buffer, counting what it was asked.
type fakeReader struct {
	data  []byte
	calls *atomic.Int64
	fail  error
}

func (f *fakeReader) ReadRange(_ context.Context, _ string, offset, length int64) ([]byte, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.calls.Add(1)
	return f.data[offset : offset+length], nil
}

// readers builds one reader per worker over the same bytes.
func readers(n int, data []byte, calls *atomic.Int64) []Reader {
	out := make([]Reader, n)
	for i := range out {
		out[i] = &fakeReader{data: data, calls: calls}
	}
	return out
}

func object(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

// TestEveryArmReadsTheWholeObject covers the property the comparison rests on:
// an arm that skipped work would be faster and wrong.
func TestEveryArmReadsTheWholeObject(t *testing.T) {
	data := object(10000)
	for _, workers := range []int{1, 3, 8} {
		var calls atomic.Int64
		p := Plan{Object: "o", Size: int64(len(data)), Block: 1024, Workers: workers}
		got, err := Run(context.Background(), "fake", readers(workers, data, &calls), p)
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		if got.Bytes != int64(len(data)) {
			t.Errorf("workers=%d read %d bytes, want %d", workers, got.Bytes, len(data))
		}
		// 10000 over 1024 is ten blocks, the last one short.
		if calls.Load() != 10 {
			t.Errorf("workers=%d made %d requests, want 10", workers, calls.Load())
		}
	}
}

// TestConcurrencyDoesNotChangeTheBytes keeps the checksum the thing that tells
// two arms apart, rather than how many readers each happened to use.
func TestConcurrencyDoesNotChangeTheBytes(t *testing.T) {
	data := object(10000)
	var calls atomic.Int64
	p := Plan{Object: "o", Size: int64(len(data)), Block: 512, Workers: 1}
	one, err := Run(context.Background(), "one", readers(1, data, &calls), p)
	if err != nil {
		t.Fatal(err)
	}
	p.Workers = 7
	many, err := Run(context.Background(), "many", readers(7, data, &calls), p)
	if err != nil {
		t.Fatal(err)
	}
	if one.Checksum != many.Checksum {
		t.Errorf("checksum %d at one worker, %d at seven", one.Checksum, many.Checksum)
	}
}

// TestADifferentBlockSizeIsADifferentChecksumOnlyIfTheBytesDiffer keeps the
// checksum a property of the object rather than of how it was requested.
func TestTheChecksumIsOfTheObjectNotTheRequests(t *testing.T) {
	data := object(9999)
	var calls atomic.Int64
	small := Plan{Object: "o", Size: int64(len(data)), Block: 256, Workers: 4}
	large := Plan{Object: "o", Size: int64(len(data)), Block: 4096, Workers: 4}
	a, err := Run(context.Background(), "small", readers(4, data, &calls), small)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(context.Background(), "large", readers(4, data, &calls), large)
	if err != nil {
		t.Fatal(err)
	}
	if a.Checksum != b.Checksum {
		t.Errorf("block size changed the checksum: %d and %d", a.Checksum, b.Checksum)
	}
}

// TestAShortArmIsAnError covers a reader that answers with fewer bytes than
// the object holds, which would otherwise be reported as a fast run.
func TestAShortArmIsAnError(t *testing.T) {
	data := object(4096)
	// The plan claims more than the reader has, so the last block comes back
	// short and the total cannot match.
	p := Plan{Object: "o", Size: 8192, Block: 4096, Workers: 1}
	rs := []Reader{&shortReader{data: data}}
	_, err := Run(context.Background(), "short", rs, p)
	if err == nil {
		t.Fatal("an arm that read half the object reported success")
	}
}

// shortReader answers every request with what it has, which is not enough.
type shortReader struct{ data []byte }

func (s *shortReader) ReadRange(_ context.Context, _ string, offset, length int64) ([]byte, error) {
	if offset >= int64(len(s.data)) {
		return nil, nil
	}
	end := offset + length
	if end > int64(len(s.data)) {
		end = int64(len(s.data))
	}
	return s.data[offset:end], nil
}

// TestAFailedReadFailsTheRun keeps a backend error from being averaged into a
// rate, since a run that did not finish has no rate.
func TestAFailedReadFailsTheRun(t *testing.T) {
	var calls atomic.Int64
	rs := []Reader{&fakeReader{fail: errors.New("backend is down"), calls: &calls}}
	p := Plan{Object: "o", Size: 4096, Block: 1024, Workers: 1}
	_, err := Run(context.Background(), "broken", rs, p)
	if err == nil {
		t.Fatal("a failed read reported success")
	}
	if !contains(err.Error(), "backend is down") {
		t.Errorf("err = %v, want the reader's own reason", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestPlanIsChecked(t *testing.T) {
	for _, c := range []struct {
		name string
		p    Plan
	}{
		{"no object", Plan{Size: 1, Block: 1, Workers: 1}},
		{"no size", Plan{Object: "o", Block: 1, Workers: 1}},
		{"no block", Plan{Object: "o", Size: 1, Workers: 1}},
		{"no workers", Plan{Object: "o", Size: 1, Block: 1}},
	} {
		if err := c.p.Validate(); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

// TestReadersMustMatchWorkers keeps an arm from running at a concurrency it
// was not given the connections for, which would report the wrong x axis.
func TestReadersMustMatchWorkers(t *testing.T) {
	var calls atomic.Int64
	p := Plan{Object: "o", Size: 1024, Block: 512, Workers: 4}
	if _, err := Run(context.Background(), "mismatch", readers(2, object(1024), &calls), p); err == nil {
		t.Error("two readers ran a four worker plan")
	}
}

func TestMedianTakesTheMiddleRun(t *testing.T) {
	rs := []Result{
		{Elapsed: 30 * time.Millisecond},
		{Elapsed: 10 * time.Millisecond},
		{Elapsed: 20 * time.Millisecond},
	}
	if got := Median(rs).Elapsed; got != 20*time.Millisecond {
		t.Errorf("median = %v, want 20ms", got)
	}
	if got := Median(nil).Elapsed; got != 0 {
		t.Errorf("median of nothing = %v", got)
	}
}

func TestRateIsMegabytesASecond(t *testing.T) {
	r := Result{Bytes: 100 << 20, Elapsed: 2 * time.Second}
	if got := r.Rate(); got < 49.9 || got > 50.1 {
		t.Errorf("rate = %v, want 50", got)
	}
	if got := (Result{Bytes: 1}).Rate(); got != 0 {
		t.Errorf("a run with no time has rate %v, want 0", got)
	}
}
