package driver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
)

// Backend is a driver held to its own declaration.
//
// The rule that a driver may not emulate a capability it lacks is enforced
// here rather than trusted: a call to something undeclared is refused before
// the driver sees it, so an accidental implementation cannot be reached and a
// deliberate one cannot be sold as the real capability.
type Backend struct {
	driver Driver
	// lister is set when the driver both declares and implements listing.
	// Held rather than asserted per call, since the answer cannot change.
	lister  Lister
	declare Declaration
	// denied holds the capabilities this credential turned out not to have.
	//
	// RFC-0016 makes a capability a property of the credential rather than of
	// the backend: a driver reporting what the store can do tells the planner
	// a dataset can be served in a way that will fail at the first read. The
	// declaration cannot be probed safely up front, since probing a write
	// writes, so it is narrowed the first time the store refuses one.
	//
	// Replaced whole rather than mutated, because Supports is on the read path
	// and narrowing happens at most once per capability: a reader takes the
	// current map and never a lock.
	denied atomic.Pointer[map[Capability]bool]
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
	b := &Backend{driver: d, declare: decl}
	if decl.Supports(ListObjects) {
		lister, ok := d.(Lister)
		if !ok {
			// Declared and not implemented is the one lie this can
			// catch for free, and catching it here means a namespace
			// never half-works: an export would otherwise list
			// nothing and give no reason.
			return nil, fmt.Errorf("%w: declares %s and does not implement Lister",
				ErrNotSupported, ListObjects)
		}
		b.lister = lister
	}
	return b, nil
}

// Declaration is what this backend claims, for a control plane to resolve
// intents against.
func (b *Backend) Declaration() Declaration { return b.declare }

// Supports answers for one capability, as this credential.
func (b *Backend) Supports(c Capability) bool {
	if d := b.denied.Load(); d != nil && (*d)[c] {
		return false
	}
	return b.declare.Supports(c)
}

// Denied lists what this credential turned out not to be allowed to do.
//
// Worth reporting rather than only acting on: a backend that quietly stopped
// offering snapshots is a dataset whose intent stopped being satisfiable for a
// reason nobody was told. RFC-0017 owns noticing it.
func (b *Backend) Denied() []Capability {
	d := b.denied.Load()
	if d == nil {
		return nil
	}
	out := make([]Capability, 0, len(*d))
	for c := range *d {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// narrow records that this credential may not do something, and returns the
// error unchanged so a caller sees what the store said.
func (b *Backend) narrow(c Capability, err error) error {
	if err == nil || !errors.Is(err, ErrDenied) {
		return err
	}
	next := map[Capability]bool{c: true}
	if cur := b.denied.Load(); cur != nil {
		for k := range *cur {
			next[k] = true
		}
	}
	b.denied.Store(&next)
	return err
}

// refuse reports a capability this backend does not have.
func refuse(c Capability) error {
	return fmt.Errorf("%w: %s", ErrNotSupported, c)
}

// ReadRange reads a byte range. Always available: it is the mandatory core,
// and Open refused any declaration lacking it.
//
// Alone among the operations it does not narrow on a refusal. A backend that
// declared the core and then withdrew it would be one Open would have refused,
// and a credential that cannot read is a broken configuration rather than a
// narrower store: it surfaces as the error it is, every time, instead of
// becoming a backend that quietly claims to do nothing.
func (b *Backend) ReadRange(ctx context.Context, object string, offset, length int64) ([]byte, error) {
	if offset < 0 || length < 0 {
		return nil, fmt.Errorf("%w: offset %d length %d", ErrRange, offset, length)
	}
	return b.driver.ReadRange(ctx, object, offset, length)
}

// List returns one level of names under a prefix, if this backend can
// enumerate at all.
//
// A limit of zero or less is refused rather than treated as unbounded: a
// prefix may hold millions, and a caller that meant everything has to say how
// much of it at a time.
func (b *Backend) List(ctx context.Context, prefix, after string, limit int) ([]Entry, error) {
	if !b.Supports(ListObjects) || b.lister == nil {
		return nil, refuse(ListObjects)
	}
	if limit <= 0 {
		return nil, fmt.Errorf("%w: a listing of %d names is not one", ErrNotSupported, limit)
	}
	out, err := b.lister.List(ctx, prefix, after, limit)
	return out, b.narrow(ListObjects, err)
}

// SizeOf reports how many bytes an object holds, if this backend can say.
func (b *Backend) SizeOf(ctx context.Context, object string) (int64, error) {
	if !b.Supports(ObjectSize) {
		return 0, refuse(ObjectSize)
	}
	out, err := b.driver.SizeOf(ctx, object)
	return out, b.narrow(ObjectSize, err)
}

// WriteObject creates an immutable object, if this backend writes at all.
func (b *Backend) WriteObject(ctx context.Context, object string, data []byte) error {
	if !b.Supports(WriteObject) {
		return refuse(WriteObject)
	}
	return b.narrow(WriteObject, b.driver.WriteObject(ctx, object, data))
}

// DeleteObject removes one.
func (b *Backend) DeleteObject(ctx context.Context, object string) error {
	if !b.Supports(DeleteObject) {
		return refuse(DeleteObject)
	}
	return b.narrow(DeleteObject, b.driver.DeleteObject(ctx, object))
}

// SnapshotObject captures a point in time the backend manages.
func (b *Backend) SnapshotObject(ctx context.Context, object string) (string, error) {
	if !b.Supports(Snapshot) {
		return "", refuse(Snapshot)
	}
	out, err := b.driver.SnapshotObject(ctx, object)
	return out, b.narrow(Snapshot, err)
}

// CloneObject makes a writable copy that shares storage rather than copying.
func (b *Backend) CloneObject(ctx context.Context, from, to string) error {
	if !b.Supports(Clone) {
		return refuse(Clone)
	}
	return b.narrow(Clone, b.driver.CloneObject(ctx, from, to))
}
