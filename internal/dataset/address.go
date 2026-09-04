// Package dataset turns what a user named into where the bytes are.
//
// Two addresses rather than one, which is the whole of RFC-0012's answer to
// its hardest question. A user names a dataset, a version and an object; the
// bytes live at an address the backend serves. Making the version part of the
// first means a deleted version cannot be named again, so cached blocks for it
// are unreachable rather than wrongly served, and nothing has to be told.
package dataset

import (
	"errors"
	"fmt"
	"strings"
)

// Ref is what a user named.
type Ref struct {
	Dataset string
	Version string
	Object  string
}

var (
	// ErrEmpty rejects a reference missing a part, since a missing version
	// would address a dataset's bytes without saying which of them.
	ErrEmpty = errors.New("dataset: a reference needs a dataset, a version and an object")
	// ErrSeparator rejects a part containing the separator, which would let
	// two different references produce one address.
	ErrSeparator = errors.New("dataset: a name may not contain a slash")
)

// separator joins the parts of an address. A name containing it is refused
// rather than escaped: escaping makes two names collide only in the cases
// nobody tests, and a slash in a dataset name buys nothing.
const separator = "/"

// Validate rejects a reference that could not address one thing.
func (r Ref) Validate() error {
	if r.Dataset == "" || r.Version == "" || r.Object == "" {
		return fmt.Errorf("%w, got %q, %q, %q", ErrEmpty, r.Dataset, r.Version, r.Object)
	}
	for name, v := range map[string]string{"dataset": r.Dataset, "version": r.Version} {
		if strings.Contains(v, separator) {
			return fmt.Errorf("%w: %s %q", ErrSeparator, name, v)
		}
	}
	return nil
}

// String is the address a user names, with the version in it.
//
// The version is here rather than alongside because that is what makes a
// deleted version unnameable: an address that omitted it would mean different
// bytes at two times, and a cached block would be valid for one of them with
// no way to tell which.
func (r Ref) String() string {
	return r.Dataset + separator + r.Version + separator + r.Object
}

// Placement says where a version's bytes actually live.
//
// A clone shares its parent's placement for everything it has not written, so
// two versions resolve to one backend address and the tier holds one copy.
// That is the case the tier is worth most in, and a cache keyed on what the
// user named would hold one copy per clone of bytes that are the same.
type Placement struct {
	// Dataset and Version are whose bytes these are, which for a clone is the
	// parent rather than the clone itself.
	Dataset, Version string
}

// Resolver maps what a user named to where the bytes are.
type Resolver interface {
	// Placement answers for one version, and reports whether it knows it. A
	// version it does not know is one that was deleted or never existed, and
	// those are the same answer to a reader.
	Placement(dataset, version string) (Placement, bool)
}

// ErrUnknown reports a version nothing can place, which is what a deleted one
// becomes.
var ErrUnknown = errors.New("dataset: no such version")

// Backend resolves a user's reference to the address a backend serves.
//
// The returned address is the cache key. A clone and its parent produce the
// same one for a shared object, so the tier holds the block once.
func Backend(r Ref, res Resolver) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	if res == nil {
		return "", fmt.Errorf("dataset: no resolver")
	}
	p, ok := res.Placement(r.Dataset, r.Version)
	if !ok {
		// A deleted version and one that never existed are one answer. A
		// reader learning which would be learning about data it cannot read.
		return "", fmt.Errorf("%w: %s/%s", ErrUnknown, r.Dataset, r.Version)
	}
	return p.Dataset + separator + p.Version + separator + r.Object, nil
}
