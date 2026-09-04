// Package driver is the seam between Forebay and a durable store.
//
// A driver reads a byte range of an immutable object. Everything else it
// declares, and the declaration is the contract: the control plane resolves
// intents against it and refuses at that point rather than when the data is
// wanted.
package driver

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// Capability names something a configured driver can do against the backend it
// was pointed at. Capabilities are additive and named, never numbered, so
// adding one cannot change what an existing declaration means.
type Capability string

const (
	// ReadRange is the whole mandatory core. A backend that can do nothing
	// else still holds datasets and serves misses.
	ReadRange Capability = "read-range"

	// ObjectSize answers how large an object is. Optional, because a store
	// that can only read a range is still a durable backend, but a caller
	// splitting an object into fixed blocks cannot tell a short answer from a
	// short object without it, and so cannot cache the last block of one.
	ObjectSize Capability = "object-size"

	WriteObject  Capability = "write-object"
	DeleteObject Capability = "delete-object"
	Snapshot     Capability = "snapshot"
	Clone        Capability = "clone"
	Replicate    Capability = "replicate"
	Thin         Capability = "thin"
	// Compresses and CompressOnRequest are separate because RFC-0020 needs
	// different answers. A store that compresses everything with no control
	// surface declares the first and not the second, and both are true.
	Compresses        Capability = "compresses"
	CompressOnRequest Capability = "compress-on-request"
	TopologyHint      Capability = "topology-hint"
	// ListObjects enumerates what a backend holds under a prefix.
	//
	// Separate from the mandatory core because most stores can and some
	// cannot, and a namespace is the one thing that cannot be built without
	// it: RFC-0008's FSAL serves an empty directory on a backend that does
	// not declare this, since inventing entries is worse than showing none.
	ListObjects Capability = "list-objects"
)

// Entry is one name under a prefix.
//
// A prefix that has objects beneath it is reported as a directory, because
// that is what a directory is in a store that has none: nothing was created to
// make it exist and nothing needs deleting to remove it.
type Entry struct {
	// Name is relative to the prefix asked for, and never contains a
	// separator: a listing returns one level, and a caller that wants the
	// next asks again.
	Name string
	// Dir says objects exist beneath this name rather than at it.
	Dir bool
	// Bytes is the object's size, and is meaningless for a directory.
	Bytes int64
}

// Lister is implemented by a driver that declares ListObjects.
//
// An interface rather than a method on Driver, so that a driver which cannot
// enumerate does not carry one that returns an error. A driver declaring the
// capability without implementing this is refused by Open, which is the same
// enforcement the mandatory core gets rather than a second kind of trust.
type Lister interface {
	// List returns one level under prefix, in name order, starting after
	// the name given and stopping at limit.
	//
	// Paged because a bucket is not a directory and a prefix may hold
	// millions: a caller that asked for everything would be asking for
	// however much somebody put there.
	List(ctx context.Context, prefix, after string, limit int) ([]Entry, error)
}

var (
	// ErrNotSupported is a refusal: the backend does not do this. Callers must
	// be able to tell it from a failure, because "I do not do that" and "I
	// could not do that just now" need different responses.
	ErrNotSupported = errors.New("driver: capability not declared")
	// ErrNoReadRange rejects a declaration missing the mandatory core.
	ErrNoReadRange = errors.New("driver: read-range is mandatory")
	// ErrRange reports a read outside the object.
	ErrRange = errors.New("driver: range is not within the object")
	// ErrDenied reports a store that can do this and a credential that may
	// not. It is a third answer beside "I do not do that" and "I could not do
	// that just now", and it is the one that does not come right on a retry:
	// RFC-0016 makes a capability a property of the credential rather than of
	// the backend, and this is how the difference arrives.
	ErrDenied = errors.New("driver: the credential is not allowed to do this")
)

// Declaration is what a configured driver says it can do.
//
// It belongs to the driver as configured, not to the technology: the same S3
// driver declares replicate against a cross-region bucket and not against a
// single one.
type Declaration struct {
	// Contract is the version of this contract the driver implements.
	Contract int
	// Capabilities is what it has. Duplicates are harmless and unknown names
	// are kept, since an older control plane must be able to drive a newer
	// driver and ignore what it does not understand.
	Capabilities []Capability
}

// Validate rejects a declaration that cannot be a backend.
func (d Declaration) Validate() error {
	if d.Contract <= 0 {
		return fmt.Errorf("driver: contract version must be positive, got %d", d.Contract)
	}
	if !d.Supports(ReadRange) {
		return ErrNoReadRange
	}
	return nil
}

// Supports answers for one capability. An unknown name is a no rather than an
// error, which is what lets a capability be added without breaking anyone.
func (d Declaration) Supports(c Capability) bool {
	return slices.Contains(d.Capabilities, c)
}

// Driver is what a backend implements. Every method beyond ReadRange may
// return ErrNotSupported, and a driver that lacks one must refuse rather than
// emulate it: a clone that copies is not a clone, and the caller chose it to
// avoid the copy.
type Driver interface {
	// Declare says what this driver can do against its configured backend.
	Declare() Declaration
	// ReadRange reads length bytes from offset. Mandatory.
	ReadRange(ctx context.Context, object string, offset, length int64) ([]byte, error)
	// SizeOf reports how many bytes an object holds.
	SizeOf(ctx context.Context, object string) (int64, error)
	// WriteObject creates an immutable object.
	WriteObject(ctx context.Context, object string, data []byte) error
	// DeleteObject removes one.
	DeleteObject(ctx context.Context, object string) error
	// SnapshotObject captures a point in time the backend manages.
	SnapshotObject(ctx context.Context, object string) (string, error)
	// CloneObject makes a writable copy that shares storage.
	CloneObject(ctx context.Context, from, to string) error
}
