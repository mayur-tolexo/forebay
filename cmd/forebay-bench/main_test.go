package main

import (
	"strings"
	"testing"
)

// TestParseWorkersSortsTheSweep matters because the widest point sets the
// connection limit the backend arm is allowed, and an unsorted sweep would
// take that from whichever point was written last.
func TestParseWorkersSortsTheSweep(t *testing.T) {
	got, err := parseWorkers("8,1,32,4")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 4, 8, 32}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parsed %v, want %v", got, want)
		}
	}
}

func TestParseWorkersRejectsWhatCannotBeASweep(t *testing.T) {
	for _, in := range []string{"", "0", "-4", "eight", "1,,2,x"} {
		if _, err := parseWorkers(in); err == nil {
			t.Errorf("parseWorkers(%q) was accepted", in)
		}
	}
}

func TestSplitListDropsEmpties(t *testing.T) {
	got := splitList(" a , ,b,")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("splitList = %q, want [a b]", got)
	}
	if got := splitList(""); got != nil {
		t.Errorf("splitList(\"\") = %q, want nothing", got)
	}
}

// TestConditionsSayWhichSideCarriesWhat covers the block RFC-0018 requires
// before a number: a result that cannot say which side carried caching is not
// reported as a locality result, so the harness has to say it.
func TestConditionsSayWhichSideCarriesWhat(t *testing.T) {
	var out strings.Builder
	conditions(&out, "shard", 1<<26, 1<<20, 3, 16, 18, 3, "")
	got := out.String()
	for _, want := range []string{"same block grid", "no compression", "16 connections", "page cache", "upper bound"} {
		if !strings.Contains(got, want) {
			t.Errorf("conditions do not mention %q:\n%s", want, got)
		}
	}

	// With an extent to evict, the page-cache caveat is replaced by the claim
	// that the tier was read from the device, and the two must not both stand.
	out.Reset()
	conditions(&out, "shard", 1<<26, 1<<20, 3, 16, 18, 3, "/pool/extent")
	got = out.String()
	if !strings.Contains(got, "evicted") {
		t.Errorf("conditions do not say the extent was evicted:\n%s", got)
	}
	if strings.Contains(got, "upper bound") {
		t.Errorf("conditions still carry the page-cache caveat after eviction:\n%s", got)
	}
}

// TestConditionsWarnWhenTheColdArmsWillRunOut keeps a sweep from silently
// dropping the arm that prices the socket.
func TestConditionsWarnWhenTheColdArmsWillRunOut(t *testing.T) {
	var out strings.Builder
	conditions(&out, "shard", 1<<26, 1<<20, 3, 16, 4, 3, "")
	if !strings.Contains(out.String(), "fewer than") {
		t.Errorf("no warning that 4 cold objects cannot feed 3 runs at 3 points:\n%s", out.String())
	}
}
