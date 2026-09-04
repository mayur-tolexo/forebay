package intent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mayur-tolexo/forebay/driver"
)

// fake declares whatever a test says it can do.
type fake struct{ caps []driver.Capability }

func (f fake) Declare() driver.Declaration {
	return driver.Declaration{Contract: 1, Capabilities: append([]driver.Capability{driver.ReadRange}, f.caps...)}
}
func (fake) ReadRange(context.Context, string, int64, int64) ([]byte, error) { return nil, nil }
func (fake) SizeOf(context.Context, string) (int64, error)                   { return 0, nil }
func (fake) WriteObject(context.Context, string, []byte) error               { return nil }
func (fake) DeleteObject(context.Context, string) error                      { return nil }
func (fake) SnapshotObject(context.Context, string) (string, error)          { return "", nil }
func (fake) CloneObject(context.Context, string, string) error               { return nil }

func backend(t *testing.T, caps ...driver.Capability) *driver.Backend {
	t.Helper()
	b, err := driver.Open(fake{caps: caps})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTheDefaultChangesNothing is the property the defaults are chosen for: a
// dataset that declares nothing gets the store's own durability and no
// borrowed capacity, which is how the system behaves uninstalled.
func TestTheDefaultChangesNothing(t *testing.T) {
	got := Intent{}.WithDefaults()
	if got.Durability != DurabilityBackend || got.Latency != LatencyBestEffort || got.Cost != CostBalanced {
		t.Errorf("defaults = %+v", got)
	}
	if len(Intent{}.Needs()) != 0 {
		t.Error("the default intent asks a backend for something beyond existing")
	}
	if err := Resolve(Intent{}, backend(t), Fleet{}); err != nil {
		t.Errorf("the default intent was unsatisfiable on a plain backend: %v", err)
	}
}

// TestAContradictionIsRefusedRatherThanResolved covers the pair that means two
// different things. Picking one silently is the degradation this project
// refuses.
func TestAContradictionIsRefusedRatherThanResolved(t *testing.T) {
	err := Intent{Cost: CostCheapest, Latency: LatencyCached}.Validate()
	if err == nil {
		t.Fatal("asking for the cheapest thing and for borrowed capacity was accepted")
	}
	for _, want := range []string{"cheapest", "cached", "two different requests"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
}

// TestAWordOutsideTheVocabularyIsRefused keeps the closed set closed, and says
// what the set is so a user can correct it.
func TestAWordOutsideTheVocabularyIsRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		i    Intent
		want string
	}{
		{"durability", Intent{Durability: "eleven-nines"}, "rack-tolerant"},
		{"latency", Intent{Latency: "instant"}, "best-effort"},
		{"cost", Intent{Cost: "free"}, "balanced"},
	} {
		err := c.i.Validate()
		if err == nil {
			t.Errorf("an invented %s was accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("the %s refusal does not list the vocabulary: %v", c.name, err)
		}
	}
}

// TestRackToleranceNeedsBothHalves is the case RFC-0009 asks for by name: one
// refusal, two causes, and neither hidden.
func TestRackToleranceNeedsBothHalves(t *testing.T) {
	want := Intent{Durability: DurabilityRackTolerant}

	// A backend that cannot replicate, on a fleet that cannot see racks.
	err := Resolve(want, backend(t), Fleet{})
	var u *Unsatisfiable
	if !errors.As(err, &u) {
		t.Fatalf("err = %v, want Unsatisfiable", err)
	}
	if len(u.Missing) != 1 || u.Missing[0] != driver.Replicate {
		t.Errorf("missing = %v, want replicate", u.Missing)
	}
	if len(u.Fleet) != 1 {
		t.Errorf("fleet reasons = %v, want the rack one", u.Fleet)
	}
	if !strings.Contains(err.Error(), "replicate") || !strings.Contains(err.Error(), "rack") {
		t.Errorf("the message hides one of the two causes: %v", err)
	}

	// A backend that can replicate, on a fleet that still cannot see racks:
	// the intent is still unsatisfiable, for a reason no backend could fix.
	err = Resolve(want, backend(t, driver.Replicate), Fleet{})
	if !errors.As(err, &u) || len(u.Missing) != 0 || len(u.Fleet) != 1 {
		t.Errorf("err = %v, want only the fleet's own blindness", err)
	}

	// Both halves present.
	if err := Resolve(want, backend(t, driver.Replicate), Fleet{KnowsRacks: true}); err != nil {
		t.Errorf("a fleet that can do it refused: %v", err)
	}
}

// TestAnUnknownRackIsNotItsOwnRack keeps the intent from being satisfied by
// assuming the thing it was asked to guarantee.
func TestAnUnknownRackIsNotItsOwnRack(t *testing.T) {
	err := Resolve(Intent{Durability: DurabilityRackTolerant}, backend(t, driver.Replicate), Fleet{KnowsRacks: false})
	if err == nil {
		t.Fatal("rack tolerance was satisfied on a fleet that cannot name a rack")
	}
}

// TestReplicatedNeedsTheBackendToSayItCan covers the capability contract: a
// declaration is trusted, and an undeclared one is refused before the driver.
func TestReplicatedNeedsTheBackendToSayItCan(t *testing.T) {
	if err := Resolve(Intent{Durability: DurabilityReplicated}, backend(t), Fleet{}); err == nil {
		t.Error("replicated resolved against a backend that does not declare it")
	}
	if err := Resolve(Intent{Durability: DurabilityReplicated}, backend(t, driver.Replicate), Fleet{}); err != nil {
		t.Errorf("replicated refused a backend that declares it: %v", err)
	}
	// Scratch asks for nothing at all.
	if err := Resolve(Intent{Durability: DurabilityNone}, backend(t), Fleet{}); err != nil {
		t.Errorf("scratch was unsatisfiable: %v", err)
	}
}

func TestResolveNeedsABackend(t *testing.T) {
	if err := Resolve(Intent{}, nil, Fleet{}); err == nil {
		t.Error("an intent resolved against no backend")
	}
}
