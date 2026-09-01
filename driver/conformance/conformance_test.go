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
	if len(found) != 4 {
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
