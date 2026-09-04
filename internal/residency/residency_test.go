package residency

import (
	"strings"
	"testing"
)

// TestALevelIsWhatASchedulerCanActOn covers the plain mapping, before any
// hysteresis: a node holding nothing, a little, or most of a dataset.
func TestALevelIsWhatASchedulerCanActOn(t *testing.T) {
	for _, c := range []struct {
		fraction float64
		want     Level
	}{
		{0, None},
		{0.1, None},
		{0.25, Some},
		{0.5, Some},
		{0.75, Most},
		{1, Most},
	} {
		if got := Next(None, c.fraction); got != c.want {
			t.Errorf("a node holding %.2f from nothing published %v, want %v", c.fraction, got, c.want)
		}
	}
}

// TestALevelHoldsBetweenTheThresholds is the hysteresis, and it is what makes
// the signal publishable at all: a node sitting near a boundary would
// otherwise rewrite its labels on every admission and eviction.
func TestALevelHoldsBetweenTheThresholds(t *testing.T) {
	// Between the some lines: 0.20 to 0.25.
	if got := Next(Some, 0.22); got != Some {
		t.Errorf("a node already at some, holding 0.22, published %v", got)
	}
	if got := Next(None, 0.22); got != None {
		t.Errorf("a node at none, holding 0.22, published %v: it has not risen yet", got)
	}

	// Between the most lines: 0.70 to 0.75.
	if got := Next(Most, 0.72); got != Most {
		t.Errorf("a node already at most, holding 0.72, published %v", got)
	}
	if got := Next(Some, 0.72); got != Some {
		t.Errorf("a node at some, holding 0.72, published %v: it has not risen yet", got)
	}
}

// TestALevelFallsWhenItReallyFalls keeps the hysteresis from becoming a
// ratchet, which would leave a reclaimed node advertising data it no longer
// holds.
func TestALevelFallsWhenItReallyFalls(t *testing.T) {
	if got := Next(Most, 0.69); got != Some {
		t.Errorf("a node at most, fallen to 0.69, published %v, want some", got)
	}
	if got := Next(Some, 0.19); got != None {
		t.Errorf("a node at some, fallen to 0.19, published %v, want none", got)
	}
	if got := Next(Most, 0); got != None {
		t.Errorf("a node that lost everything published %v", got)
	}
}

// TestFlappingAtABoundaryWritesNothing is the property the margin exists for,
// stated as the sequence an operator would actually see.
func TestFlappingAtABoundaryWritesNothing(t *testing.T) {
	current := Next(None, 0.30)
	if current != Some {
		t.Fatalf("a node holding 0.30 published %v", current)
	}
	for _, f := range []float64{0.26, 0.24, 0.21, 0.23, 0.26, 0.22} {
		next := Next(current, f)
		if next != current {
			t.Errorf("holding %.2f moved the level from %v to %v", f, current, next)
		}
		current = next
	}
}

// TestNoneCarriesNoLabel matters because a node holding nothing should have no
// label rather than one saying so, which would be a key per dataset on every
// node in the cluster.
func TestNoneCarriesNoLabel(t *testing.T) {
	if got := None.String(); got != "" {
		t.Errorf("none published the value %q", got)
	}
	if Some.String() == "" || Most.String() == "" {
		t.Error("a level that should be published has no value")
	}
	if Level(99).String() != "" {
		t.Error("a level that does not exist published a value")
	}
}

// TestTheKeyFitsALabel covers the bound that forced hashing in the first
// place: a label key's name is limited and tenant and dataset names are not.
func TestTheKeyFitsALabel(t *testing.T) {
	long := strings.Repeat("x", 200)
	key := Key(long, long)

	name := key[strings.Index(key, "/")+1:]
	if len(name) > 63 {
		t.Errorf("the label name is %d characters, want at most 63: %q", len(name), name)
	}
	if !strings.HasPrefix(key, "forebay.io/") {
		t.Errorf("the key is not namespaced: %q", key)
	}
	if rack := RackKey(long, long); rack == key {
		t.Error("the rack key is the same as the node key")
	}
	if name := RackKey(long, long); len(name[strings.Index(name, "/")+1:]) > 63 {
		t.Errorf("the rack label name is too long: %q", name)
	}
}

// TestTheKeySeparatesTenantFromDataset matters because a tenant and dataset
// whose names run together must not collide with a different pair that
// concatenates the same way.
func TestTheKeySeparatesTenantFromDataset(t *testing.T) {
	if Key("ab", "cd") == Key("abc", "d") {
		t.Error("two different tenant and dataset pairs share a key")
	}
	if Key("a", "b") != Key("a", "b") {
		t.Error("the same pair produced two keys")
	}
}

// TestFractionRefusesWhatItCannotDivide covers the inputs that would otherwise
// make an empty dataset a warm start everywhere, or hide a disagreement about
// how large a dataset is.
func TestFractionRefusesWhatItCannotDivide(t *testing.T) {
	for _, c := range []struct {
		name            string
		resident, total int64
	}{
		{"a dataset of no bytes", 0, 0},
		{"a negative size", 1, -1},
		{"negative residency", -1, 10},
		{"more resident than exists", 11, 10},
	} {
		if _, err := Fraction(c.resident, c.total); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}

	got, err := Fraction(3, 4)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0.75 {
		t.Errorf("3 of 4 bytes is %v, want 0.75", got)
	}
}
