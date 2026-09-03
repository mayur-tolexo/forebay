package agent

import (
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/pool"
)

// TestRateIgnoresWhatTheAgentDid is the property the whole duration form rests
// on. Free space rises when the agent reclaims, and reading that as the
// workload's consumption would shrink the floor in the pass that had just
// proved it too small.
func TestRateIgnoresWhatTheAgentDid(t *testing.T) {
	base := time.Unix(0, 0)
	var r rateEstimator

	// A first pass has nothing to compare against.
	if _, known := r.observe(100<<20, 40<<20, base); known {
		t.Error("the first observation reported a rate")
	}

	// The agent reclaimed 10 MiB and the workload wrote nothing: free rose by
	// exactly what was returned, and consumption is zero.
	got, known := r.observe(110<<20, 30<<20, base.Add(time.Second))
	if !known {
		t.Fatal("the second observation reported no rate")
	}
	if got != 0 {
		t.Errorf("a pure reclaim measured %.0f B/s of consumption, want 0", got)
	}
}

// TestRateSeesTheWorkloadThroughAReclaim covers the case that matters: both
// happen at once, and only the workload's half is the rate.
func TestRateSeesTheWorkloadThroughAReclaim(t *testing.T) {
	base := time.Unix(0, 0)
	var r rateEstimator
	r.observe(100<<20, 40<<20, base)

	// The workload took 8 MiB while the agent gave back 10 MiB, so free rose
	// by 2 MiB. The rate is the 8 MiB, not zero and not a negative.
	got, _ := r.observe(102<<20, 30<<20, base.Add(2*time.Second))
	if want := float64(4 << 20); got != want {
		t.Errorf("rate = %.0f B/s over two seconds, want %.0f", got, want)
	}
}

// TestRateIsNeverNegative covers a workload that deleted more than it wrote,
// which is not consumption and must not size a floor below the minimum.
func TestRateIsNeverNegative(t *testing.T) {
	base := time.Unix(0, 0)
	var r rateEstimator
	r.observe(100<<20, 40<<20, base)
	if got, _ := r.observe(180<<20, 40<<20, base.Add(time.Second)); got != 0 {
		t.Errorf("freed space measured %.0f B/s of consumption, want 0", got)
	}
}

// TestRateNeedsTimeToPass keeps two observations at the same instant from
// dividing by zero.
func TestRateNeedsTimeToPass(t *testing.T) {
	base := time.Unix(0, 0)
	var r rateEstimator
	r.observe(100<<20, 0, base)
	if _, known := r.observe(90<<20, 0, base); known {
		t.Error("two samples at the same instant reported a rate")
	}
}

// TestTargetFromADuration covers the conversion, including the minimum that
// covers a quiet node and the first pass, which has no rate at all.
func TestTargetFromADuration(t *testing.T) {
	cfg := WatchConfig{HeadroomFor: 2 * time.Second, MinHeadroom: 1 << 20, Interval: time.Second}

	if got := cfg.target(0, false); got != 1<<20 {
		t.Errorf("a pass with no rate kept %s, want the minimum", got)
	}
	if got := cfg.target(0, true); got != 1<<20 {
		t.Errorf("a quiet node kept %s, want the minimum", got)
	}
	// 8 MiB/s for two seconds is 16 MiB, which is above the minimum.
	if got, want := cfg.target(8<<20, true), pool.Bytes(16<<20); got != want {
		t.Errorf("target = %s, want %s", got, want)
	}
	// A trickle stays at the floor rather than below it.
	if got := cfg.target(1<<10, true); got != 1<<20 {
		t.Errorf("a trickle kept %s, want the minimum", got)
	}
}

// TestAFixedHeadroomIsUsedAsItStands keeps the old form working unchanged.
func TestAFixedHeadroomIsUsedAsItStands(t *testing.T) {
	cfg := WatchConfig{Headroom: 4 << 20, Interval: time.Second}
	for _, rate := range []float64{0, 1 << 30} {
		if got := cfg.target(rate, true); got != 4<<20 {
			t.Errorf("a fixed headroom moved to %s at %.0f B/s", got, rate)
		}
	}
}

// TestWatchConfigIsChecked covers the configurations that would leave a node
// with no floor, or with two.
func TestWatchConfigIsChecked(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  WatchConfig
	}{
		{"neither form", WatchConfig{Interval: time.Second}},
		{"both forms", WatchConfig{Headroom: 1, HeadroomFor: time.Second, MinHeadroom: 1, Interval: time.Second}},
		{"a duration with no minimum", WatchConfig{HeadroomFor: time.Second, Interval: time.Second}},
		{"no interval", WatchConfig{Headroom: 1}},
	} {
		if err := c.cfg.Validate(); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
	for _, c := range []struct {
		name string
		cfg  WatchConfig
	}{
		{"a size", WatchConfig{Headroom: 1 << 20, Interval: time.Second}},
		{"a duration", WatchConfig{HeadroomFor: time.Second, MinHeadroom: 1 << 20, Interval: time.Second}},
	} {
		if err := c.cfg.Validate(); err != nil {
			t.Errorf("%s was refused: %v", c.name, err)
		}
	}
}

// TestRateOverestimatesIfFreeSpaceLagsAReclaim pins what happens when the
// filesystem has not yet reflected an unlink by the next poll.
//
// The correction adds back what the agent returned, so if free space has not
// risen by it yet the difference is counted as the workload's. The floor comes
// out too large rather than too small, which is the safe direction, and it
// rests on an assumption RFC-0018 has an open row for: when freed capacity
// becomes observable, rather than when unlink returns.
func TestRateOverestimatesIfFreeSpaceLagsAReclaim(t *testing.T) {
	base := time.Unix(0, 0)
	var r rateEstimator
	r.observe(100<<20, 40<<20, base)

	// 10 MiB reclaimed and the workload wrote nothing, but free space has not
	// moved yet. Accounting has: borrowed already fell.
	got, _ := r.observe(100<<20, 30<<20, base.Add(time.Second))
	if want := float64(10 << 20); got != want {
		t.Errorf("rate = %.0f B/s, want %.0f: the lag is attributed to the workload", got, want)
	}
}
