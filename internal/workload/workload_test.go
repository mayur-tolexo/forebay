package workload

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestConfigIsChecked(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  Config
	}{
		{"no directory", Config{Block: 4096, Interval: time.Second, Limit: 1 << 20}},
		{"no block", Config{Dir: "/tmp", Interval: time.Second, Limit: 1 << 20}},
		{"no interval", Config{Dir: "/tmp", Block: 4096, Limit: 1 << 20}},
		{"a limit smaller than a block", Config{Dir: "/tmp", Block: 8192, Interval: time.Second, Limit: 4096}},
	} {
		if err := c.cfg.Validate(); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

// TestUnalignedBlocksAreRefused covers the block size direct IO would reject
// with EINVAL, which is clearer refused up front than as a failed write.
func TestUnalignedBlocksAreRefused(t *testing.T) {
	if _, err := alignedBuffer(4097); err == nil {
		t.Error("an unaligned block was accepted")
	}
	buf, err := alignedBuffer(8192)
	if err != nil {
		t.Fatal(err)
	}
	if len(buf) != 8192 {
		t.Errorf("buffer is %d bytes, want 8192", len(buf))
	}
}

// TestRunSamplesTheIntervals checks the writer banks progress rather than
// reporting one total, since a stall inside a total cannot be seen.
func TestRunSamplesTheIntervals(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("direct IO needs Linux")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()

	got, err := Run(ctx, Config{Dir: t.TempDir(), Block: 1 << 16, Interval: 100 * time.Millisecond, Limit: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 3 {
		t.Fatalf("banked %d samples in 700ms of 100ms intervals, want several", len(got))
	}
	for i, s := range got {
		if s.Bytes == 0 {
			t.Errorf("sample %d wrote nothing", i)
		}
		if s.Rate() <= 0 {
			t.Errorf("sample %d has rate %v", i, s.Rate())
		}
		if s.Slowest <= 0 {
			t.Errorf("sample %d recorded no write time", i)
		}
	}
}

// TestRunRefusesADirectoryItCannotWrite keeps a failed setup from being
// reported as a workload that achieved nothing.
func TestRunRefusesADirectoryItCannotWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := Run(ctx, Config{Dir: "/nonexistent-forebay", Block: 4096, Interval: time.Millisecond, Limit: 1 << 20}); err == nil {
		t.Error("writing to a directory that is not there succeeded")
	}
}

// samples builds a run with known shape for the summarising helpers.
func samples() []Sample {
	return []Sample{
		{Start: 0, Bytes: 100 << 20, Elapsed: time.Second, Slowest: 2 * time.Millisecond},
		{Start: time.Second, Bytes: 100 << 20, Elapsed: time.Second, Slowest: 3 * time.Millisecond},
		{Start: 2 * time.Second, Bytes: 20 << 20, Elapsed: time.Second, Slowest: 400 * time.Millisecond},
		{Start: 3 * time.Second, Bytes: 100 << 20, Elapsed: time.Second, Slowest: 2 * time.Millisecond},
	}
}

// TestBetweenTakesOverlappingIntervals matters because a disturbance rarely
// lines up with an interval boundary, and an interval it partly covers is
// still an interval it affected.
func TestBetweenTakesOverlappingIntervals(t *testing.T) {
	got := Between(samples(), 2100*time.Millisecond, 2200*time.Millisecond)
	if len(got) != 1 || got[0].Start != 2*time.Second {
		t.Errorf("got %d samples starting %v, want the third", len(got), got)
	}
	if n := len(Between(samples(), 0, 4*time.Second)); n != 4 {
		t.Errorf("a window over the whole run took %d samples, want 4", n)
	}
	if n := len(Between(samples(), 10*time.Second, 11*time.Second)); n != 0 {
		t.Errorf("a window past the run took %d samples", n)
	}
}

// TestTheDentShowsInTheWorstStall is the property the experiment turns on: a
// median hides one bad interval, and the maximum is where it appears.
func TestTheDentShowsInTheWorstStall(t *testing.T) {
	all := samples()
	if got, want := MedianRate(all), 100.0; got < want-1 || got > want+1 {
		t.Errorf("median rate over a run with one bad interval = %.1f, want about %.0f", got, want)
	}
	if got := WorstStall(all); got != 400*time.Millisecond {
		t.Errorf("worst stall = %v, want 400ms", got)
	}
	if got := MedianRate(nil); got != 0 {
		t.Errorf("median of nothing = %v", got)
	}
	if got := WorstStall(nil); got != 0 {
		t.Errorf("worst stall of nothing = %v", got)
	}
}

// TestSamplerBanksOnTheInterval covers the bookkeeping without a device, since
// the writing needs Linux and the arithmetic does not.
func TestSamplerBanksOnTheInterval(t *testing.T) {
	base := time.Unix(0, 0)
	s := newSampler(base, 100*time.Millisecond)

	// Two writes inside the first interval, the second slower.
	s.record(1000, 2*time.Millisecond, base.Add(30*time.Millisecond))
	s.record(1000, 9*time.Millisecond, base.Add(60*time.Millisecond))
	if len(s.samples) != 0 {
		t.Fatalf("banked %d samples before the interval closed", len(s.samples))
	}
	// This one crosses the boundary and closes it.
	s.record(1000, 1*time.Millisecond, base.Add(110*time.Millisecond))
	if len(s.samples) != 1 {
		t.Fatalf("banked %d samples, want 1", len(s.samples))
	}
	got := s.samples[0]
	if got.Bytes != 3000 {
		t.Errorf("interval holds %d bytes, want 3000", got.Bytes)
	}
	if got.Slowest != 9*time.Millisecond {
		t.Errorf("slowest = %v, want 9ms", got.Slowest)
	}
	if got.Start != 0 || got.Elapsed != 110*time.Millisecond {
		t.Errorf("interval spans %v for %v, want 0 for 110ms", got.Start, got.Elapsed)
	}
}

// TestSamplerKeepsTheTail matters because a disturbance can land in the last,
// short interval, and dropping it would lose exactly the evidence wanted.
func TestSamplerKeepsTheTail(t *testing.T) {
	base := time.Unix(0, 0)
	s := newSampler(base, time.Second)
	s.record(4096, time.Millisecond, base.Add(10*time.Millisecond))

	out := s.close(base.Add(20 * time.Millisecond))
	if len(out) != 1 {
		t.Fatalf("closed with %d samples, want the partial one kept", len(out))
	}
	if out[0].Bytes != 4096 || out[0].Elapsed != 20*time.Millisecond {
		t.Errorf("tail = %d bytes over %v", out[0].Bytes, out[0].Elapsed)
	}
	// Closing again must not bank an empty interval.
	if again := s.close(base.Add(30 * time.Millisecond)); len(again) != 1 {
		t.Errorf("closing twice banked %d samples", len(again))
	}
}

// TestPhasesSplitTheRun covers the boundaries the experiment turns on: an
// interval a disturbance partly covers belongs to during, and the tail after
// it belongs to after however short the run's end is.
func TestPhasesSplitTheRun(t *testing.T) {
	before, during, after := Phases(samples(), 2100*time.Millisecond, 2200*time.Millisecond)
	if len(before) != 3 {
		t.Errorf("before holds %d samples, want 3", len(before))
	}
	if len(during) != 1 || during[0].Start != 2*time.Second {
		t.Errorf("during holds %d samples, want the third", len(during))
	}
	if len(after) != 2 {
		t.Errorf("after holds %d samples, want 2", len(after))
	}
}

// TestPhasesWithNothingDone is the control arm's shape: from and to are the
// same instant, and every sample is still accounted for.
func TestPhasesWithNothingDone(t *testing.T) {
	at := 2100 * time.Millisecond
	before, during, after := Phases(samples(), at, at)
	if len(during) != 0 {
		t.Errorf("during holds %d samples for a zero window, want none", len(during))
	}
	if len(before)+len(after) < len(samples()) {
		t.Errorf("before %d and after %d lose samples from a run of %d",
			len(before), len(after), len(samples()))
	}
}
