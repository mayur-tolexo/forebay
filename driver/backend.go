package driver

import (
	"context"
	"fmt"
)

// Backend is a driver held to its own declaration.
//
// The rule that a driver may not emulate a capability it lacks is enforced
// here rather than trusted: a call to something undeclared is refused before
// the driver sees it, so an accidental implementation cannot be reached and a
// deliberate one cannot be sold as the real capability.
type Backend struct {
	driver  Driver
	declare Declaration
}

// Open holds a driver to what it declares.
func Open(d Driver) (*Backend, error) {
	if d == nil {
		return nil, fmt.Errorf("driver: no driver")
	}
	decl := d.Declare()
	if err := decl.Validate(); err != nil {
		return nil, err
	}
	return &Backend{driver: d, declare: decl}, nil
}

// Declaration is what this backend claims, for a control plane to resolve
// intents against.
func (b *Backend) Declaration() Declaration { return b.declare }

// Supports answers for one capability.
func (b *Backend) Supports(c Capability) bool { return b.declare.Supports(c) }

// refuse reports a capability this backend does not have.
func refuse(c Capability) error {
	return fmt.Errorf("%w: %s", ErrNotSupported, c)
}

// ReadRange reads a byte range. Always available: it is the mandatory core,
// and Open refused any declaration lacking it.
func (b *Backend) ReadRange(ctx context.Context, object string, offset, length int64) ([]byte, error) {
	if offset < 0 || length < 0 {
		return nil, fmt.Errorf("%w: offset %d length %d", ErrRange, offset, length)
	}
	return b.driver.ReadRange(ctx, object, offset, length)
}

// SizeOf reports how many bytes an object holds, if this backend can say.
func (b *Backend) SizeOf(ctx context.Context, object string) (int64, error) {
	if !b.Supports(ObjectSize) {
		return 0, refuse(ObjectSize)
	}
	return b.driver.SizeOf(ctx, object)
}

// WriteObject creates an immutable object, if this backend writes at all.
func (b *Backend) WriteObject(ctx context.Context, object string, data []byte) error {
	if !b.Supports(WriteObject) {
		return refuse(WriteObject)
	}
	return b.driver.WriteObject(ctx, object, data)
}

// DeleteObject removes one.
func (b *Backend) DeleteObject(ctx context.Context, object string) error {
	if !b.Supports(DeleteObject) {
		return refuse(DeleteObject)
	}
	return b.driver.DeleteObject(ctx, object)
}

// SnapshotObject captures a point in time the backend manages.
func (b *Backend) SnapshotObject(ctx context.Context, object string) (string, error) {
	if !b.Supports(Snapshot) {
		return "", refuse(Snapshot)
	}
	return b.driver.SnapshotObject(ctx, object)
}

// CloneObject makes a writable copy that shares storage rather than copying.
func (b *Backend) CloneObject(ctx context.Context, from, to string) error {
	if !b.Supports(Clone) {
		return refuse(Clone)
	}
	return b.driver.CloneObject(ctx, from, to)
}
