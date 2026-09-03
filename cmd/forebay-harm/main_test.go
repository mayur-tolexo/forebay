package main

import "testing"

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
