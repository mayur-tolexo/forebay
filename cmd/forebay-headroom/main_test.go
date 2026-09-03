package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mayur-tolexo/forebay/internal/pool"
)

// readings plays a fixed sequence of free-space readings, one per call, and
// holds the last once it runs out.
func readings(values ...pool.Bytes) func() (pool.Bytes, error) {
	// Guarded, because measure asks from the watch and from the tracker at
	// once, which is the same demand the real reader has to meet.
	var (
		mu sync.Mutex
		i  int
	)
	return func() (pool.Bytes, error) {
		mu.Lock()
		defer mu.Unlock()
		v := values[i]
		if i < len(values)-1 {
			i++
		}
		return v, nil
	}
}

// TestTrackFindsTheWorstDeficit covers the number the experiment reports. The
// deficit is opened and closed between two of the watch's passes, so what
// matters is the lowest reading rather than the last.
func TestTrackFindsTheWorstDeficit(t *testing.T) {
	const target = pool.Bytes(1000)
	free := readings(1200, 900, 700, 950, 1100, 1100)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	worst, below, count := track(ctx, free, target, 10*time.Millisecond)

	if worst != 300 {
		t.Errorf("worst deficit = %s, want 300B: the lowest reading was 700", worst)
	}
	if below <= 0 {
		t.Error("time below the target was not accumulated")
	}
	if count < 5 {
		t.Errorf("took %d samples, want at least the six readings", count)
	}
}

// TestTrackReportsNothingWhenTheTargetHolds keeps a run in which the watch kept
// up from reporting a deficit it did not have.
func TestTrackReportsNothingWhenTheTargetHolds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	worst, below, _ := track(ctx, readings(2000, 1800, 1500), 1000, 10*time.Millisecond)

	if worst != 0 {
		t.Errorf("worst deficit = %s over readings that never crossed, want 0", worst)
	}
	if below != 0 {
		t.Errorf("time below = %s, want none", below)
	}
}

// TestTrackDoesNotCountTimeItWasNotBelow matters because a deficit that opens
// and closes twice must not have the gap between counted as time below.
func TestTrackDoesNotCountTimeItWasNotBelow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// Below, then recovered for a long stretch, then below again, then
	// recovered for the rest: the helper holds its last value, so ending above
	// the target is what keeps the run from drifting back under it.
	free := readings(900, 5000, 5000, 5000, 5000, 5000, 900, 900, 5000)
	_, below, _ := track(ctx, free, 1000, 10*time.Millisecond)

	// Only the adjacent below-target samples contribute, so the recovered
	// stretch must not be inside the total.
	if below > 60*time.Millisecond {
		t.Errorf("time below = %s, which includes the stretch it was above", below)
	}
}

// TestTrackSurvivesAFailedReading keeps one unreadable filesystem poll from
// ending the measurement.
func TestTrackSurvivesAFailedReading(t *testing.T) {
	var i int
	free := func() (pool.Bytes, error) {
		i++
		if i == 2 {
			return 0, context.DeadlineExceeded
		}
		return 500, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	worst, _, count := track(ctx, free, 1000, 10*time.Millisecond)

	if worst != 500 {
		t.Errorf("worst deficit = %s, want 500B", worst)
	}
	if count < 3 {
		t.Errorf("a failed reading ended the measurement after %d samples", count)
	}
}

// TestTargetForRefusesABudgetTheDiskCannotCover covers the case that would
// otherwise surface as the watch refusing a headroom of zero, which reads as a
// configuration mistake rather than as a disk with less free space than asked.
func TestTargetForRefusesABudgetTheDiskCannotCover(t *testing.T) {
	if _, err := targetFor(1<<30, 4<<30); err == nil {
		t.Error("a budget larger than the free space was accepted")
	}
	if _, err := targetFor(1<<30, 1<<30); err == nil {
		t.Error("a budget whose half is the whole free space was accepted")
	}
	got, err := targetFor(10<<30, 2<<30)
	if err != nil {
		t.Fatal(err)
	}
	if want := pool.Bytes(8 << 30); got != want {
		t.Errorf("target = %s, want %s: half the budget under what is free", got, want)
	}
}

// good is a configuration that should be accepted, so each case below differs
// from a usable one in exactly the thing it names.
func good() options {
	return options{
		Borrowed: "/pool", Journal: "/journal",
		Capacity: 100, Lend: 50, Budget: 10, TargetUnder: 4, Leases: 4, Writers: 1,
		Block: 4096, Interval: time.Second, Sample: time.Millisecond, RunFor: time.Second,
	}
}

func TestCheckFlags(t *testing.T) {
	if err := checkFlags(good()); err != nil {
		t.Fatalf("a usable configuration was refused: %v", err)
	}
	for _, c := range []struct {
		name string
		edit func(*options)
	}{
		{"no pool", func(o *options) { o.Borrowed = "" }},
		{"no journal", func(o *options) { o.Journal = "" }},
		{"no capacity", func(o *options) { o.Capacity = 0 }},
		{"no leases", func(o *options) { o.Leases = 0 }},
		{"lending more than the node has", func(o *options) { o.Lend = 500 }},
		{"sampling slower than the watch", func(o *options) { o.Sample = time.Second }},
	} {
		o := good()
		c.edit(&o)
		if err := checkFlags(o); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

// TestMeasureRunsWithoutADevice drives the whole measurement over a free space
// function that falls and then recovers, so the flow is exercised where direct
// IO is not available and the answer is one the test decides.
func TestMeasureRunsWithoutADevice(t *testing.T) {
	// The journal lives outside the pool, since startup reaps the pool and
	// refuses to open one holding something it would delete.
	root := t.TempDir()
	dir := filepath.Join(root, "borrowed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	o := options{
		Borrowed: dir, Journal: filepath.Join(root, "leases.json"),
		Capacity: 8 << 20, Lend: 4 << 20, Budget: 2 << 20, TargetUnder: 1 << 20, Leases: 4,
		Writers: 0, Block: 4096,
		Interval: 40 * time.Millisecond, Sample: 5 * time.Millisecond, RunFor: 250 * time.Millisecond,
	}
	// Starts above the target, dips a megabyte under it, then recovers.
	free := readings(8<<20, 8<<20, 6<<20, 6<<20, 8<<20)

	got, err := measure(context.Background(), o, free)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if want := pool.Bytes(7 << 20); got.Target != want {
		t.Errorf("target = %s, want %s", got.Target, want)
	}
	if want := pool.Bytes(1 << 20); got.Deficit != want {
		t.Errorf("deficit = %s, want %s", got.Deficit, want)
	}
	if got.Samples == 0 {
		t.Error("nothing was sampled")
	}
}

// pool builds a usable pool directory and a journal beside it.
func poolDirs(t *testing.T) (dir, journal string) {
	t.Helper()
	root := t.TempDir()
	dir = filepath.Join(root, "borrowed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, filepath.Join(root, "leases.json")
}

// TestMeasureReportsWhyItCouldNotStart covers the three ways the setup fails,
// since each would otherwise surface as a run that measured nothing.
func TestMeasureReportsWhyItCouldNotStart(t *testing.T) {
	dir, journal := poolDirs(t)
	base := options{
		Borrowed: dir, Journal: journal,
		Capacity: 8 << 20, Lend: 4 << 20, Budget: 2 << 20, TargetUnder: 1 << 20, Leases: 4,
		Block: 4096, Interval: 40 * time.Millisecond, Sample: 5 * time.Millisecond,
		RunFor: 50 * time.Millisecond,
	}

	t.Run("a journal inside the pool", func(t *testing.T) {
		// Startup reaps the pool, so a journal in it would be deleted by the
		// run that depends on it.
		o := base
		o.Journal = filepath.Join(dir, "leases.json")
		if _, err := measure(context.Background(), o, readings(8<<20)); err == nil {
			t.Error("a journal inside the pool was accepted")
		}
	})

	t.Run("lending more than the node has", func(t *testing.T) {
		o := base
		o.Lend = 64 << 20
		if _, err := measure(context.Background(), o, readings(8<<20)); err == nil {
			t.Error("granting more than the capacity succeeded")
		}
	})

	t.Run("a budget the disk cannot cover", func(t *testing.T) {
		dir, journal := poolDirs(t)
		o := base
		o.Borrowed, o.Journal = dir, journal
		// Free space smaller than half the budget leaves no target.
		if _, err := measure(context.Background(), o, readings(1<<20)); err == nil {
			t.Error("a budget larger than the free space was accepted")
		}
	})

	t.Run("a filesystem that will not say", func(t *testing.T) {
		dir, journal := poolDirs(t)
		o := base
		o.Borrowed, o.Journal = dir, journal
		free := func() (pool.Bytes, error) { return 0, errors.New("no statfs") }
		if _, err := measure(context.Background(), o, free); err == nil {
			t.Error("a free space reader that failed was treated as zero free")
		}
	})
}

// TestReportStatesTheConditions covers the output, which is the experiment's
// deliverable: a headroom figure quoted without the poll interval it was taken
// at is not a headroom figure.
func TestReportStatesTheConditions(t *testing.T) {
	r := report{Start: 10 << 30, Target: 8 << 30, Deficit: 1536 << 20, Below: 1234 * time.Millisecond, Samples: 42}
	o := good()
	o.Interval, o.Sample, o.Writers = 2*time.Second, 50*time.Millisecond, 3

	var out strings.Builder
	if err := r.writeTo(&out, o); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1.50GiB", "2s", "50ms", "3 writer(s)", "1.234s", "42"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not state %q:\n%s", want, out.String())
		}
	}
}

// TestReportSaysWhenTheBudgetBoundedIt covers the warning that separates a
// deficit the watch produced from one the configuration did: writers that
// reach their budget stop taking space, and the deficit stops growing with
// them rather than with the watch.
func TestReportSaysWhenTheBudgetBoundedIt(t *testing.T) {
	o := good()
	o.Budget = 100

	var out strings.Builder
	if err := (report{Took: 100}).writeTo(&out, o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "bounded by the budget") {
		t.Errorf("a run that reached its budget did not say so:\n%s", out.String())
	}

	out.Reset()
	if err := (report{Took: 40}).writeTo(&out, o); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "bounded by the budget") {
		t.Errorf("a run well inside its budget was called budget bound:\n%s", out.String())
	}
}
