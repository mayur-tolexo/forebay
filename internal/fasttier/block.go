// Package fasttier caches immutable content on borrowed capacity.
//
// The content it holds cannot change, so there is no invalidation and no
// coherence protocol: a changed object is a different object. What can happen
// is the capacity underneath being taken back, and a reader must then see a
// miss rather than an error or stale bytes.
package fasttier

import "fmt"

// BlockRef names content. Identity includes the version, which is what makes a
// cached block unable to disagree with the backend.
type BlockRef struct {
	// Backend is the durable store the content came from.
	Backend string
	// Object identifies an immutable object, version included.
	Object string
	// Index is which fixed-size block of that object this is.
	Index int64
}

func (b BlockRef) String() string {
	return fmt.Sprintf("%s/%s#%d", b.Backend, b.Object, b.Index)
}

// Key is a block scoped to a tenant.
//
// Blocks are not shared between tenants even when the content is identical:
// sharing would reveal that two tenants hold the same bytes, and the capacity
// saved does not pay for an inference channel.
type Key struct {
	Tenant string
	Block  BlockRef
}

func (k Key) String() string { return k.Tenant + ":" + k.Block.String() }
