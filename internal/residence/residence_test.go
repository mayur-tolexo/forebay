package residence

import (
	"errors"
	"strings"
	"testing"
)

// TestAnUnconfinedVersionMayBeAnywhereNamed covers the ordinary case: most
// data has no residency requirement, and requiring one would make the rule
// something operators route around.
func TestAnUnconfinedVersionMayBeAnywhereNamed(t *testing.T) {
	if err := (Policy{}).Permits("eu-west-1"); err != nil {
		t.Errorf("an unconfined version was refused a region: %v", err)
	}
}

// TestAConfinedVersionStaysWhereItWasConfined is the rule itself.
func TestAConfinedVersionStaysWhereItWasConfined(t *testing.T) {
	p := Policy{Allowed: []Region{"eu-west-1", "eu-central-1"}}

	if err := p.Permits("eu-central-1"); err != nil {
		t.Errorf("an allowed region was refused: %v", err)
	}
	err := p.Permits("us-east-1")
	if !errors.Is(err, ErrNotAllowed) {
		t.Errorf("a region outside the allowed set gave %v", err)
	}
	// The message names what was allowed, since an operator reading a refusal
	// needs to know what it was measured against.
	if !strings.Contains(err.Error(), "eu-central-1, eu-west-1") {
		t.Errorf("the refusal does not say what is allowed: %v", err)
	}
}

// TestDenyWinsOverAllow matters because the alternative lets adding a
// permission silently remove a prohibition.
func TestDenyWinsOverAllow(t *testing.T) {
	p := Policy{
		Allowed: []Region{"eu-west-1", "us-east-1"},
		Denied:  []Region{"us-east-1"},
	}
	if err := p.Permits("us-east-1"); !errors.Is(err, ErrDenied) {
		t.Errorf("a region both allowed and denied gave %v, want denied", err)
	}
	if err := p.Permits("eu-west-1"); err != nil {
		t.Errorf("a region allowed and not denied was refused: %v", err)
	}
}

// TestADenialAloneLeavesTheRestOpen covers how a single prohibited
// jurisdiction is usually expressed: exclude one, do not enumerate the world.
func TestADenialAloneLeavesTheRestOpen(t *testing.T) {
	p := Policy{Denied: []Region{"us-east-1"}}
	if err := p.Permits("ap-south-1"); err != nil {
		t.Errorf("a region merely not denied was refused: %v", err)
	}
	if err := p.Permits("us-east-1"); !errors.Is(err, ErrDenied) {
		t.Errorf("the denied region gave %v", err)
	}
}

// TestAnUnnamedRegionIsRefused is how data reaches somewhere nobody meant to
// include, if unknown is treated as permission.
func TestAnUnnamedRegionIsRefused(t *testing.T) {
	for _, r := range []Region{"", " ", "\t"} {
		if err := (Policy{}).Permits(r); !errors.Is(err, ErrUnnamed) {
			t.Errorf("region %q gave %v, want a refusal", r, err)
		}
	}
	// Even against a policy that names it as allowed, since what is unnamed
	// cannot be the thing that was named.
	if err := (Policy{Allowed: []Region{""}}).Permits(""); !errors.Is(err, ErrUnnamed) {
		t.Errorf("an unnamed region allowed by name gave %v", err)
	}
}

// TestATransferChecksBothEnds matters because a version that should not have
// been at the origin is already a breach, and letting it be the source would
// spread one mistake rather than report it.
func TestATransferChecksBothEnds(t *testing.T) {
	p := Policy{Allowed: []Region{"eu-west-1"}}

	if err := Transfer(p, "eu-west-1", "eu-west-1"); err != nil {
		t.Errorf("a transfer within the allowed region was refused: %v", err)
	}

	err := Transfer(p, "us-east-1", "eu-west-1")
	if err == nil {
		t.Fatal("a transfer out of a region the version may not be in was allowed")
	}
	if !strings.Contains(err.Error(), "origin") {
		t.Errorf("the refusal does not say which end failed: %v", err)
	}

	err = Transfer(p, "eu-west-1", "us-east-1")
	if err == nil {
		t.Fatal("a transfer into a region outside the allowed set was permitted")
	}
	if !strings.Contains(err.Error(), "destination") {
		t.Errorf("the refusal does not say which end failed: %v", err)
	}
}

// TestATightenedRuleReportsWhatIsNowWrong is why the same question answers
// both "may it move here" and "should it be here": a rule that only bound
// future transfers would leave existing copies quietly grandfathered.
func TestATightenedRuleReportsWhatIsNowWrong(t *testing.T) {
	at := []Region{"us-east-1", "eu-west-1", "ap-south-1"}

	if got := Breaches(Policy{}, at); len(got) != 0 {
		t.Errorf("an unconfined version was in breach at %v", got)
	}

	got := Breaches(Policy{Allowed: []Region{"eu-west-1"}}, at)
	want := []Region{"ap-south-1", "us-east-1"}
	if len(got) != len(want) {
		t.Fatalf("breaches = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("breaches = %v, want %v in that order", got, want)
		}
	}
}
