package dataset

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mayur-tolexo/forebay/driver"
)

// fixed places whatever a test says exists.
type fixed map[string]Placement

func (f fixed) Placement(dataset, version string) (Placement, bool) {
	p, ok := f[dataset+"/"+version]
	return p, ok
}

// TestTheVersionIsInTheAddress is what makes a deleted version unnameable, and
// so what makes cached blocks for it unreachable rather than wrongly served.
func TestTheVersionIsInTheAddress(t *testing.T) {
	a := Ref{Dataset: "corpus", Version: "v3", Object: "shard-0"}
	b := Ref{Dataset: "corpus", Version: "v4", Object: "shard-0"}
	if a.String() == b.String() {
		t.Fatal("two versions of one object produced one address")
	}
	if !strings.Contains(a.String(), "v3") {
		t.Errorf("the address does not carry the version: %q", a.String())
	}
}

// TestACloneSharesItsParentsCacheKey is the case the tier is worth most in: a
// hundred experiments against one corpus should hold one copy of each block.
func TestACloneSharesItsParentsCacheKey(t *testing.T) {
	// Two clones, both sharing the golden corpus.
	res := fixed{
		"exp-1/v1":  {Dataset: "corpus", Version: "v3"},
		"exp-2/v1":  {Dataset: "corpus", Version: "v3"},
		"corpus/v3": {Dataset: "corpus", Version: "v3"},
	}
	one, err := Backend(Ref{Dataset: "exp-1", Version: "v1", Object: "shard-0"}, res)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Backend(Ref{Dataset: "exp-2", Version: "v1", Object: "shard-0"}, res)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := Backend(Ref{Dataset: "corpus", Version: "v3", Object: "shard-0"}, res)
	if err != nil {
		t.Fatal(err)
	}
	if one != two || one != parent {
		t.Errorf("clones did not share the parent's key: %q, %q, %q", one, two, parent)
	}
}

// TestADeletedVersionCannotBeNamed covers the answer to this document's
// hardest question: nothing has to be told, because the address stops
// resolving.
func TestADeletedVersionCannotBeNamed(t *testing.T) {
	res := fixed{"corpus/v3": {Dataset: "corpus", Version: "v3"}}
	if _, err := Backend(Ref{Dataset: "corpus", Version: "v3", Object: "s"}, res); err != nil {
		t.Fatalf("a live version did not resolve: %v", err)
	}
	// v2 was deleted, or never existed: one answer either way.
	_, err := Backend(Ref{Dataset: "corpus", Version: "v2", Object: "s"}, res)
	if !errors.Is(err, ErrUnknown) {
		t.Errorf("err = %v, want ErrUnknown", err)
	}
}

// TestAReferenceMustAddressOneThing keeps a name that contains the separator
// from letting two references produce one address.
func TestAReferenceMustAddressOneThing(t *testing.T) {
	for _, c := range []struct {
		name string
		ref  Ref
		want error
	}{
		{"no dataset", Ref{Version: "v1", Object: "o"}, ErrEmpty},
		{"no version", Ref{Dataset: "d", Object: "o"}, ErrEmpty},
		{"no object", Ref{Dataset: "d", Version: "v1"}, ErrEmpty},
		{"a slash in the dataset", Ref{Dataset: "a/b", Version: "v1", Object: "o"}, ErrSeparator},
		{"a slash in the version", Ref{Dataset: "d", Version: "v/1", Object: "o"}, ErrSeparator},
	} {
		if err := c.ref.Validate(); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	// An object may contain a slash, since a shard is a path within a version.
	if err := (Ref{Dataset: "d", Version: "v1", Object: "a/b/c"}).Validate(); err != nil {
		t.Errorf("a path inside a version was refused: %v", err)
	}
}

func TestBackendNeedsAResolver(t *testing.T) {
	if _, err := Backend(Ref{Dataset: "d", Version: "v", Object: "o"}, nil); err == nil {
		t.Error("a reference resolved with no resolver")
	}
}

// caps declares what a test says.
type caps struct{ list []driver.Capability }

func (c caps) Declare() driver.Declaration {
	return driver.Declaration{Contract: 1, Capabilities: append([]driver.Capability{driver.ReadRange}, c.list...)}
}
func (caps) ReadRange(context.Context, string, int64, int64) ([]byte, error) { return nil, nil }
func (caps) SizeOf(context.Context, string) (int64, error)                   { return 0, nil }
func (caps) WriteObject(context.Context, string, []byte) error               { return nil }
func (caps) DeleteObject(context.Context, string) error                      { return nil }
func (caps) SnapshotObject(context.Context, string) (string, error)          { return "", nil }
func (caps) CloneObject(context.Context, string, string) error               { return nil }

// TestCloneIsRefusedByCapabilityName keeps an operator from reading that
// cloning is unsupported, which says nothing about what to change.
func TestCloneIsRefusedByCapabilityName(t *testing.T) {
	open := func(cs ...driver.Capability) *driver.Backend {
		b, err := driver.Open(caps{list: cs})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if err := CanClone(open(driver.Clone)); err != nil {
		t.Errorf("a backend that declares clone was refused: %v", err)
	}
	err := CanClone(open(driver.Snapshot))
	if err == nil || !strings.Contains(err.Error(), "clone that copies is not a clone") {
		t.Errorf("snapshot-only backend: %v", err)
	}
	err = CanClone(open())
	if err == nil || !strings.Contains(err.Error(), "neither snapshot nor clone") {
		t.Errorf("plain backend: %v", err)
	}
	if err := CanClone(nil); err == nil {
		t.Error("a nil backend could clone")
	}
}
