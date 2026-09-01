package driver_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mayur-tolexo/forebay/driver"
)

// eager implements everything and declares only what it is told to, so a call
// reaching it proves the guard did not refuse.
type eager struct {
	caps    []driver.Capability
	reached map[driver.Capability]bool
}

func newEager(caps ...driver.Capability) *eager {
	return &eager{caps: caps, reached: map[driver.Capability]bool{}}
}

func (e *eager) Declare() driver.Declaration {
	return driver.Declaration{Contract: 1, Capabilities: e.caps}
}
func (e *eager) ReadRange(context.Context, string, int64, int64) ([]byte, error) {
	e.reached[driver.ReadRange] = true
	return []byte("ok"), nil
}
func (e *eager) WriteObject(context.Context, string, []byte) error {
	e.reached[driver.WriteObject] = true
	return nil
}
func (e *eager) DeleteObject(context.Context, string) error {
	e.reached[driver.DeleteObject] = true
	return nil
}
func (e *eager) SnapshotObject(context.Context, string) (string, error) {
	e.reached[driver.Snapshot] = true
	return "snap", nil
}
func (e *eager) CloneObject(context.Context, string, string) error {
	e.reached[driver.Clone] = true
	return nil
}

func TestAnUndeclaredCapabilityNeverReachesTheDriver(t *testing.T) {
	// The rule that a driver may not emulate what it lacks is enforced rather
	// than trusted: an implementation that exists but was not declared cannot
	// be called, so it cannot be sold as the real capability.
	d := newEager(driver.ReadRange)
	b, err := driver.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, c := range []struct {
		cap driver.Capability
		try func() error
	}{
		{driver.WriteObject, func() error { return b.WriteObject(ctx, "o", nil) }},
		{driver.DeleteObject, func() error { return b.DeleteObject(ctx, "o") }},
		{driver.Snapshot, func() error { _, err := b.SnapshotObject(ctx, "o"); return err }},
		{driver.Clone, func() error { return b.CloneObject(ctx, "a", "b") }},
	} {
		if err := c.try(); !errors.Is(err, driver.ErrNotSupported) {
			t.Errorf("%s = %v, want ErrNotSupported", c.cap, err)
		}
		if d.reached[c.cap] {
			t.Errorf("%s reached the driver despite not being declared", c.cap)
		}
	}
}

func TestADeclaredCapabilityIsPassedThrough(t *testing.T) {
	d := newEager(driver.ReadRange, driver.WriteObject, driver.Snapshot)
	b, err := driver.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.WriteObject(context.Background(), "o", nil); err != nil {
		t.Errorf("a declared write was refused: %v", err)
	}
	if _, err := b.SnapshotObject(context.Background(), "o"); err != nil {
		t.Errorf("a declared snapshot was refused: %v", err)
	}
	if !d.reached[driver.WriteObject] || !d.reached[driver.Snapshot] {
		t.Error("declared capabilities did not reach the driver")
	}
}

func TestOpenRefusesADeclarationItCannotUse(t *testing.T) {
	for _, c := range []struct {
		name string
		decl driver.Declaration
		want error
	}{
		{"no read-range", driver.Declaration{Contract: 1, Capabilities: []driver.Capability{driver.WriteObject}}, driver.ErrNoReadRange},
		{"no contract version", driver.Declaration{Capabilities: []driver.Capability{driver.ReadRange}}, nil},
	} {
		if err := c.decl.Validate(); err == nil {
			t.Errorf("%s: Validate = nil, want a refusal", c.name)
		} else if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: Validate = %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := driver.Open(nil); err == nil {
		t.Error("Open(nil) succeeded")
	}
}

func TestAnUnknownCapabilityIsANoRatherThanAnError(t *testing.T) {
	// This is what lets an older control plane drive a newer driver: a name it
	// does not understand is simply not used.
	d := driver.Declaration{Contract: 1, Capabilities: []driver.Capability{driver.ReadRange, "invented-later"}}
	if err := d.Validate(); err != nil {
		t.Errorf("a declaration carrying an unknown name was refused: %v", err)
	}
	if d.Supports("never-heard-of-it") {
		t.Error("an unknown capability was claimed")
	}
	if !d.Supports("invented-later") {
		t.Error("a declared capability this build does not name was dropped")
	}
}

func TestANegativeRangeIsRefusedBeforeTheDriver(t *testing.T) {
	d := newEager(driver.ReadRange)
	b, _ := driver.Open(d)
	if _, err := b.ReadRange(context.Background(), "o", -1, 4); !errors.Is(err, driver.ErrRange) {
		t.Errorf("negative offset = %v, want ErrRange", err)
	}
	if d.reached[driver.ReadRange] {
		t.Error("a negative range reached the driver")
	}
}
