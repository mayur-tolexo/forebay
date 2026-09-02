package main

import "testing"

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
