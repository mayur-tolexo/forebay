package main

import (
	"strings"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/workload"
)

// TestCheckFlags covers the configurations that would measure something other
// than what the experiment claims: an unloaded device, a block direct IO
// rejects, or more capacity lent than the node has.
func TestCheckFlags(t *testing.T) {
	if err := checkFlags("/pool", "/journal", 100, 50, 4, 1, 4096); err != nil {
		t.Fatalf("a usable configuration was refused: %v", err)
	}
	for _, c := range []struct {
		name string
		err  error
	}{
		{"no pool", checkFlags("", "/j", 100, 50, 4, 1, 4096)},
		{"no journal", checkFlags("/p", "", 100, 50, 4, 1, 4096)},
		{"no capacity", checkFlags("/p", "/j", 0, 50, 4, 1, 4096)},
		{"nothing lent", checkFlags("/p", "/j", 100, 0, 4, 1, 4096)},
		{"no leases", checkFlags("/p", "/j", 100, 50, 0, 1, 4096)},
		{"lending more than the node has", checkFlags("/p", "/j", 10, 50, 4, 1, 4096)},
		{"no writers", checkFlags("/p", "/j", 100, 50, 4, 0, 4096)},
		{"an unaligned block", checkFlags("/p", "/j", 100, 50, 4, 1, 4097)},
	} {
		if c.err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

// TestPhasesSumAcrossWriters covers the table the experiment is read from. One
// writer's median says nothing about whether the device was busy, and the
// window has to appear beside the rate: a during column drawn from two
// intervals is a different kind of number from a before column drawn from four
// hundred.
func TestPhasesSumAcrossWriters(t *testing.T) {
	s := []workload.Sample{
		{Bytes: 100 << 20, Elapsed: time.Second, Slowest: 3 * time.Millisecond},
		{Bytes: 100 << 20, Elapsed: time.Second, Slowest: 9 * time.Millisecond},
	}
	var out strings.Builder
	phases(&out, 4, []phase{{"before", s, 2 * time.Second}, {"during", nil, 4 * time.Millisecond}})
	got := out.String()

	// 100 MiB/s each across four writers is 400.
	for _, want := range []string{"before", "400.0", "9ms", "2s", "during", "4ms"} {
		if !strings.Contains(got, want) {
			t.Errorf("the table does not carry %q:\n%s", want, got)
		}
	}
}
