package conformance_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mayur-tolexo/forebay/driver"
	"github.com/mayur-tolexo/forebay/driver/conformance"
)

// honest serves a fixed object and declares exactly what it does.
type honest struct {
	caps    []driver.Capability
	content []byte
	// lies makes an undeclared capability succeed, which is what the suite's
	// second half exists to find.
	lies bool
}

func (h *honest) Declare() driver.Declaration {
	return driver.Declaration{Contract: 1, Capabilities: h.caps}
}

func (h *honest) ReadRange(_ context.Context, _ string, offset, length int64) ([]byte, error) {
	if offset+length > int64(len(h.content)) {
		return nil, fmt.Errorf("%w: past the end", driver.ErrRange)
	}
	return h.content[offset : offset+length], nil
}

func (h *honest) refuse(c driver.Capability) error {
	if h.lies {
		return nil // Quietly does it anyway.
	}
	return fmt.Errorf("%w: %s", driver.ErrNotSupported, c)
}

func (h *honest) SizeOf(context.Context, string) (int64, error) {
	if err := h.refuse(driver.ObjectSize); err != nil {
		return 0, err
	}
	return int64(len(h.content)), nil
}
func (h *honest) WriteObject(context.Context, string, []byte) error {
	return h.refuse(driver.WriteObject)
}
func (h *honest) DeleteObject(context.Context, string) error {
	return h.refuse(driver.DeleteObject)
}
func (h *honest) SnapshotObject(context.Context, string) (string, error) {
	return "", h.refuse(driver.Snapshot)
}
func (h *honest) CloneObject(context.Context, string, string) error {
	return h.refuse(driver.Clone)
}

// check runs the suite and returns what it found.
func check(d driver.Driver) []error {
	return conformance.Check(conformance.Fixture{
		Driver: d, Object: "o", Content: []byte("abcdefgh"), WritablePrefix: "w",
	})
}

func TestTheSuitePassesAnHonestDriver(t *testing.T) {
	d := &honest{caps: []driver.Capability{driver.ReadRange}, content: []byte("abcdefgh")}
	if found := check(d); len(found) != 0 {
		t.Errorf("an honest read-only driver failed the suite: %v", found)
	}
}

func TestTheSuiteCatchesAnUndeclaredCapabilityThatWorks(t *testing.T) {
	// The half usually forgotten. A driver that quietly does something it did
	// not declare is worse than one that fails, because the control plane has
	// not planned for it.
	d := &honest{caps: []driver.Capability{driver.ReadRange}, content: []byte("abcdefgh"), lies: true}
	found := check(d)
	if len(found) == 0 {
		t.Fatal("the suite passed a driver that does what it says it cannot")
	}
	// One finding per undeclared capability the driver quietly performs.
	if len(found) != 5 {
		t.Errorf("the suite reported %d findings, want one per undeclared capability: %v", len(found), found)
	}
}

func TestADeclarationWithoutReadRangeIsRefused(t *testing.T) {
	// The mandatory core is the one thing a backend cannot be without.
	_, err := driver.Open(&honest{caps: []driver.Capability{driver.WriteObject}})
	if err == nil || !strings.Contains(err.Error(), "read-range") {
		t.Errorf("Open = %v, want a refusal naming read-range", err)
	}
}

// readOnlyLiar reads and declares object-size, and reports a size that is
// nothing like the object's.
type readOnlyLiar struct{ content []byte }

func (r *readOnlyLiar) Declare() driver.Declaration {
	return driver.Declaration{
		Contract:     1,
		Capabilities: []driver.Capability{driver.ReadRange, driver.ObjectSize},
	}
}
func (r *readOnlyLiar) SizeOf(context.Context, string) (int64, error) { return 999999, nil }
func (r *readOnlyLiar) ReadRange(_ context.Context, _ string, off, n int64) ([]byte, error) {
	if off+n > int64(len(r.content)) {
		return nil, driver.ErrRange
	}
	return r.content[off : off+n], nil
}
func (r *readOnlyLiar) WriteObject(context.Context, string, []byte) error {
	return driver.ErrNotSupported
}
func (r *readOnlyLiar) DeleteObject(context.Context, string) error { return driver.ErrNotSupported }
func (r *readOnlyLiar) SnapshotObject(context.Context, string) (string, error) {
	return "", driver.ErrNotSupported
}
func (r *readOnlyLiar) CloneObject(context.Context, string, string) error {
	return driver.ErrNotSupported
}

func TestASizeIsCheckedOnABackendThatCannotWrite(t *testing.T) {
	// A store that only reads is the shape most likely to declare
	// object-size, and a check living behind write-object would never run
	// against it. The number is trusted to decide that a short block is a
	// whole block, so a wrong one is cached as though it were complete.
	found := check(&readOnlyLiar{content: []byte("abcdefgh")})
	if len(found) != 1 {
		t.Fatalf("want the lie caught, got %d findings: %v", len(found), found)
	}
	if !strings.Contains(found[0].Error(), "object-size") {
		t.Errorf("caught something else: %v", found[0])
	}
}
