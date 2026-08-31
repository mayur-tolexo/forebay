package version

import "testing"

func TestStringIncludesAllThreeFields(t *testing.T) {
	Version, Commit, Date = "v0.1.0", "abc1234", "2026-08-31"
	want := "v0.1.0 (abc1234, built 2026-08-31)"
	if got := String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
