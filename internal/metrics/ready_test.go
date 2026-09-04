package metrics

import (
	"strings"
	"testing"
	"time"
)

func ready(t *testing.T) *Readiness {
	t.Helper()
	r, err := NewReadiness(100*time.Millisecond, 50*time.Millisecond, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestAQuietNodeIsReady covers the state an idle cluster is in. Holding a node
// out until it proves itself would keep every node out at once, and there
// would be nowhere for the work to go.
func TestAQuietNodeIsReady(t *testing.T) {
	if ok, _ := ready(t).Ready(time.Unix(0, 0)); !ok {
		t.Error("a node that has answered nothing was held out of service")
	}
}

// TestASlowReadTakesTheNodeOut is the failure this exists for: it passed every
// liveness probe while every client waited on it.
func TestASlowReadTakesTheNodeOut(t *testing.T) {
	r, at := ready(t), time.Unix(0, 0)
	r.Observe(10*time.Millisecond, at)
	if ok, _ := r.Ready(at); !ok {
		t.Fatal("a fast read took the node out")
	}
	r.Observe(150*time.Millisecond, at.Add(time.Second))
	ok, why := r.Ready(at.Add(time.Second))
	if ok {
		t.Fatal("a read well over the bound left the node in service")
	}
	for _, want := range []string{"150ms", "100ms bound"} {
		if !strings.Contains(why, want) {
			t.Errorf("the reason does not say %q: %q", want, why)
		}
	}
}

// TestItDoesNotFlapAcrossOneBound is why there are two. A node crossing a
// single bound alternately is removed and restored repeatedly, and every
// removal moves work that then moves back.
func TestItDoesNotFlapAcrossOneBound(t *testing.T) {
	r, at := ready(t), time.Unix(0, 0)
	r.Observe(150*time.Millisecond, at)
	if ok, _ := r.Ready(at); ok {
		t.Fatal("the node did not go out")
	}
	// Between the two bounds: better than the failure bound, not yet good
	// enough to come back.
	at = at.Add(2 * time.Minute)
	r.Observe(75*time.Millisecond, at)
	if ok, _ := r.Ready(at); ok {
		t.Error("the node recovered at a latency between the two bounds, which is where it would flap")
	}
	// Under the recovery bound.
	at = at.Add(2 * time.Minute)
	r.Observe(10*time.Millisecond, at)
	if ok, _ := r.Ready(at); !ok {
		t.Error("a node answering quickly again stayed out of service")
	}
}

// TestWhatHappenedMinutesAgoStopsCountingCoveres the window: a node held out
// by history it has already recovered from never comes back.
func TestWhatHappenedMinutesAgoStopsCounting(t *testing.T) {
	r, at := ready(t), time.Unix(0, 0)
	r.Observe(150*time.Millisecond, at)
	if ok, _ := r.Ready(at); ok {
		t.Fatal("the node did not go out")
	}
	// Nothing new, but the slow read has aged out of the window.
	later := at.Add(2 * time.Minute)
	if ok, _ := r.Ready(later); !ok {
		t.Error("a node was held out by a read that had left the window")
	}
}

// TestBoundsThatCouldNotSettleAreRefused covers the configuration that would
// reintroduce the flapping the second bound exists to prevent.
func TestBoundsThatCouldNotSettleAreRefused(t *testing.T) {
	for _, c := range []struct {
		name                  string
		fail, recover, window time.Duration
	}{
		{"no bounds", 0, 0, time.Minute},
		{"no recovery bound", time.Second, 0, time.Minute},
		{"recovering at the failing bound", time.Second, time.Second, time.Minute},
		{"recovering above the failing bound", time.Second, 2 * time.Second, time.Minute},
		{"no window", time.Second, time.Millisecond, 0},
	} {
		if _, err := NewReadiness(c.fail, c.recover, c.window); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}
