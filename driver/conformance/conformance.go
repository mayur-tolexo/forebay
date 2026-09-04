// Package conformance is the suite a driver has to pass.
//
// It is importable so that somebody can write a driver for a store this
// project has never seen and demonstrate it works, without us reviewing it.
package conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/driver"
)

// Fixture is what a driver author supplies: a driver ready to use, and an
// object of known content already present in its backend.
type Fixture struct {
	// Driver is the configured driver under test.
	Driver driver.Driver
	// Object names something readable, registered in place rather than written
	// by the suite, since a read-only backend is a legitimate backend.
	Object string
	// Content is what that object holds, so ranged reads can be checked
	// against it rather than against themselves.
	Content []byte
	// WritablePrefix names objects the suite may create. A unique suffix is
	// added to each run, because a backend that writes but cannot delete would
	// otherwise pass once and fail forever after on a name it left behind.
	WritablePrefix string
}

// session is one run of the suite, and remembers what it created so it can
// try to put the backend back as it found it.
type session struct {
	fixture Fixture
	name    string
	made    []string
}

// scratch names an object this run may create, unique to the run.
func (s *session) scratch(n int) string {
	return fmt.Sprintf("%s-%s-%d", s.fixture.WritablePrefix, s.name, n)
}

// cleanup removes what the run created, as far as the backend allows.
//
// Best effort by necessity: a store that cannot delete keeps them, which is a
// property of that backend rather than a failure of the driver, so it is not
// reported as one.
func (s *session) cleanup(b *driver.Backend) {
	if !b.Supports(driver.DeleteObject) {
		return
	}
	for _, o := range s.made {
		_ = b.DeleteObject(context.Background(), o)
	}
}

// Run checks a driver against its own declaration.
//
// Two halves, and the second is the one usually forgotten: what the driver
// claims must work, and what it does not claim must be refused cleanly rather
// than half work.
func Run(t *testing.T, f Fixture) {
	t.Helper()
	for _, err := range Check(f) {
		t.Error(err)
	}
}

// Check runs the suite and returns what it found, so a driver author can run
// it against a real backend outside `go test`, and so the suite itself can be
// tested against drivers that are wrong on purpose.
func Check(f Fixture) []error {
	if len(f.Content) < 4 {
		return []error{fmt.Errorf("fixture content is %d bytes, too short to test range boundaries", len(f.Content))}
	}
	b, err := driver.Open(f.Driver)
	if err != nil {
		return []error{fmt.Errorf("opening the driver: %w", err)}
	}
	s := &session{fixture: f, name: strconv.FormatInt(time.Now().UnixNano(), 36)}
	defer s.cleanup(b)

	var found []error
	for _, check := range []func(*driver.Backend, *session) []error{
		declaration, ranges, sizes, refusals, declared, listing,
	} {
		found = append(found, check(b, s)...)
	}
	return found
}

// listing checks what a driver that declares it must answer alike, whether it
// is a filesystem with real directories or a store that has none.
//
// The shape is the point. A namespace built on one backend has to work on the
// other, so both report a level: names directly under a prefix, directories
// among them, and nothing from further down.
func listing(b *driver.Backend, s *session) []error {
	ctx := context.Background()
	if !b.Supports(driver.ListObjects) {
		// Refused rather than empty. An export that cannot tell "no
		// listing here" from "nothing here" shows a client an empty
		// directory for a dataset that exists.
		if _, err := b.List(ctx, "", "", 10); !errors.Is(err, driver.ErrNotSupported) {
			return []error{fmt.Errorf("listing is not declared and List gave %v, want a refusal", err)}
		}
		return nil
	}

	var found []error
	// A limit is required: a prefix may hold millions, and a caller that
	// meant everything has to say how much of it at a time.
	if _, err := b.List(ctx, "", "", 0); err == nil {
		found = append(found, errors.New("a listing of zero names was accepted"))
	}

	// The fixture is somewhere, so the root has at least one name in it.
	root, err := b.List(ctx, "", "", 1000)
	if err != nil {
		return append(found, fmt.Errorf("listing the root: %w", err))
	}
	if len(root) == 0 {
		return append(found, errors.New("the root lists nothing, though the fixture object is in it"))
	}
	for _, e := range root {
		if e.Name == "" {
			found = append(found, errors.New("a listing carried a name that is empty"))
		}
		if strings.Contains(e.Name, "/") {
			found = append(found, fmt.Errorf("%q is more than one level, so a listing returned what is beneath it", e.Name))
		}
	}
	if !sort.SliceIsSorted(root, func(i, j int) bool { return root[i].Name < root[j].Name }) {
		found = append(found, errors.New("a listing is not in name order, so paging on the last name seen would skip or repeat"))
	}

	// Paging: asking for one, then asking again after it, must not repeat.
	first, err := b.List(ctx, "", "", 1)
	if err != nil {
		return append(found, fmt.Errorf("listing one name: %w", err))
	}
	if len(first) != 1 {
		found = append(found, fmt.Errorf("a limit of one returned %d names", len(first)))
	} else {
		next, err := b.List(ctx, "", first[0].Name, 1000)
		if err != nil {
			found = append(found, fmt.Errorf("listing after %q: %w", first[0].Name, err))
		}
		for _, e := range next {
			if e.Name <= first[0].Name {
				found = append(found, fmt.Errorf("listing after %q returned %q, which is not after it", first[0].Name, e.Name))
			}
		}
	}

	// A prefix nothing is under is an empty level rather than an error:
	// there is nothing in an object store for it to be missing.
	if _, err := b.List(ctx, s.scratch(9000)+"-nowhere", "", 10); err != nil {
		found = append(found, fmt.Errorf("listing a prefix nothing is under: %w", err))
	}
	return found
}

// declaration checks the contract itself is answerable.
func declaration(b *driver.Backend, _ *session) []error {
	var found []error
	d := b.Declaration()
	if d.Contract <= 0 {
		found = append(found, fmt.Errorf("contract version is %d, want a positive version", d.Contract))
	}
	// An unknown name answers no rather than failing, which is what lets a
	// newer driver be driven by an older control plane.
	if b.Supports(driver.Capability("something-invented-later")) {
		found = append(found, errors.New("an unknown capability was claimed"))
	}
	return found
}

// ranges checks the one operation every backend must have, at the edges where
// off-by-one errors live and where the fast tier will lean hardest.
func ranges(b *driver.Backend, s *session) []error {
	f := s.fixture
	var found []error
	ctx := context.Background()
	size := int64(len(f.Content))

	for _, c := range []struct {
		name           string
		offset, length int64
	}{
		{"whole object", 0, size},
		{"first byte", 0, 1},
		{"last byte", size - 1, 1},
		{"middle", 1, size - 2},
		{"empty range", 0, 0},
	} {
		got, err := b.ReadRange(ctx, f.Object, c.offset, c.length)
		if err != nil {
			found = append(found, fmt.Errorf("%s: %w", c.name, err))
			continue
		}
		if want := f.Content[c.offset : c.offset+c.length]; !bytes.Equal(got, want) {
			found = append(found, fmt.Errorf("%s: read %q, want %q", c.name, got, want))
		}
	}

	// Past the end is an error rather than a short read, because a caller that
	// asked for a range and got fewer bytes cannot tell that from truncation.
	if _, err := b.ReadRange(ctx, f.Object, size, 1); err == nil {
		found = append(found, errors.New("reading past the end succeeded, want an error"))
	}
	if _, err := b.ReadRange(ctx, f.Object, 0, size+1); err == nil {
		found = append(found, errors.New("reading more than the object holds succeeded, want an error"))
	}
	// A negative range is refused before the driver sees it.
	if _, err := b.ReadRange(ctx, f.Object, -1, 1); !errors.Is(err, driver.ErrRange) {
		found = append(found, fmt.Errorf("negative offset = %v, want ErrRange", err))
	}
	return found
}

// refusals checks the half that is usually forgotten. An undeclared capability
// that half works is worse than one that fails, because the control plane will
// not have planned for it.
//
// Against the driver rather than the backend, deliberately. The backend refuses
// an undeclared capability before the driver is reached, so asking it would
// test that guard and pass for any driver at all. What has to be established
// is that the driver itself refuses, since Driver is the interface a third
// party implements and can be called directly.
// sizes checks object-size against the fixture's own object.
//
// Deliberately not folded in with the capabilities that need a write. A store
// that only reads is the shape most likely to declare this one, and a check
// that lives behind write-object never runs against it.
func sizes(b *driver.Backend, s *session) []error {
	if !b.Supports(driver.ObjectSize) {
		return nil
	}
	// Exact, not an upper bound. A caller uses this to decide that a short
	// block is a whole block: too small and it caches a fragment as if it
	// were complete, too large and it asks for bytes that are not there.
	switch got, err := b.SizeOf(context.Background(), s.fixture.Object); {
	case err != nil:
		return []error{fmt.Errorf("object-size is declared but failed: %w", err)}
	case got != int64(len(s.fixture.Content)):
		return []error{fmt.Errorf("object-size said %s holds %d bytes, the fixture says %d", s.fixture.Object, got, len(s.fixture.Content))}
	}
	return nil
}

func refusals(_ *driver.Backend, s *session) []error {
	f := s.fixture
	var found []error
	ctx := context.Background()
	decl := f.Driver.Declare()
	for _, c := range []struct {
		cap driver.Capability
		try func() error
	}{
		{driver.WriteObject, func() error {
			// Recorded before the attempt: a driver that quietly writes has
			// created the object whether or not it admits to it.
			s.made = append(s.made, s.scratch(1))
			return f.Driver.WriteObject(ctx, s.scratch(1), []byte("x"))
		}},
		{driver.DeleteObject, func() error { return f.Driver.DeleteObject(ctx, s.scratch(1)) }},
		{driver.ObjectSize, func() error { _, err := f.Driver.SizeOf(ctx, f.Object); return err }},
		{driver.Snapshot, func() error { _, err := f.Driver.SnapshotObject(ctx, f.Object); return err }},
		{driver.Clone, func() error {
			s.made = append(s.made, s.scratch(2))
			return f.Driver.CloneObject(ctx, f.Object, s.scratch(2))
		}},
	} {
		if decl.Supports(c.cap) {
			continue
		}
		err := c.try()
		if err == nil {
			found = append(found, fmt.Errorf("%s is not declared but succeeded, which is worse than failing", c.cap))
			continue
		}
		// A refusal has to be distinguishable from a failure: the caller
		// responds differently to "I do not do that" and "not just now".
		if !errors.Is(err, driver.ErrNotSupported) {
			found = append(found, fmt.Errorf("%s refused with %v, which a caller cannot tell from a transient failure", c.cap, err))
		}
	}
	return found
}

// declared exercises what the driver does claim, since a declaration is
// trusted and therefore has to be true.
func declared(b *driver.Backend, s *session) []error {
	var found []error
	ctx := context.Background()
	if !b.Supports(driver.WriteObject) {
		return nil
	}
	object := s.scratch(3)
	body := []byte("written by the conformance suite")
	s.made = append(s.made, object)
	if err := b.WriteObject(ctx, object, body); err != nil {
		return append(found, fmt.Errorf("write-object is declared but failed: %w", err))
	}
	got, err := b.ReadRange(ctx, object, 0, int64(len(body)))
	if err != nil {
		return append(found, fmt.Errorf("reading back what was written: %w", err))
	}
	if !bytes.Equal(got, body) {
		found = append(found, fmt.Errorf("read back %q, want %q", got, body))
	}
	if b.Supports(driver.DeleteObject) {
		if err := b.DeleteObject(ctx, object); err != nil {
			found = append(found, fmt.Errorf("delete-object is declared but failed: %w", err))
		}
		if _, err := b.ReadRange(ctx, object, 0, 1); err == nil {
			found = append(found, errors.New("the object survived a delete the driver said it did"))
		}
	}
	return found
}
