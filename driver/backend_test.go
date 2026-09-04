package driver_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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

// SizeOf answers, because an eager driver claims everything.
func (e *eager) SizeOf(context.Context, string) (int64, error) { return 0, nil }

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

// picky refuses whatever a test says this credential may not do, the way a
// store refuses a caller whose policy does not allow it.
type picky struct {
	driver.Driver
	deny map[driver.Capability]bool
	// calls counts what actually reached the driver, which is how a test sees
	// a capability that stopped being offered.
	calls map[driver.Capability]int
}

func (p *picky) note(c driver.Capability) error {
	if p.calls == nil {
		p.calls = map[driver.Capability]int{}
	}
	p.calls[c]++
	if p.deny[c] {
		return fmt.Errorf("%w: policy says no", driver.ErrDenied)
	}
	return nil
}

func (p *picky) Declare() driver.Declaration {
	return driver.Declaration{Contract: 1, Capabilities: []driver.Capability{
		driver.ReadRange, driver.ObjectSize, driver.WriteObject, driver.DeleteObject, driver.Snapshot, driver.Clone,
	}}
}
func (p *picky) ReadRange(context.Context, string, int64, int64) ([]byte, error) {
	return nil, p.note(driver.ReadRange)
}
func (p *picky) SizeOf(context.Context, string) (int64, error) { return 0, p.note(driver.ObjectSize) }
func (p *picky) WriteObject(context.Context, string, []byte) error {
	return p.note(driver.WriteObject)
}
func (p *picky) DeleteObject(context.Context, string) error { return p.note(driver.DeleteObject) }
func (p *picky) SnapshotObject(context.Context, string) (string, error) {
	return "", p.note(driver.Snapshot)
}
func (p *picky) CloneObject(context.Context, string, string) error { return p.note(driver.Clone) }

// TestACapabilityBelongsToTheCredential is RFC-0016's answer: a driver that
// reported what the store can do would tell the planner a dataset can be
// served in a way that fails at the first read.
func TestACapabilityBelongsToTheCredential(t *testing.T) {
	d := &picky{deny: map[driver.Capability]bool{driver.Snapshot: true}}
	b, err := driver.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Declared, so it is offered until the store says otherwise.
	if !b.Supports(driver.Snapshot) {
		t.Fatal("a declared capability was not offered")
	}
	if _, err := b.SnapshotObject(ctx, "o"); !errors.Is(err, driver.ErrDenied) {
		t.Fatalf("the refusal did not reach the caller: %v", err)
	}
	// And now it is not offered, so nothing plans against it.
	if b.Supports(driver.Snapshot) {
		t.Error("a capability this credential was refused is still offered")
	}
	// A second call is refused here rather than reaching the store, which is
	// the saving: one round trip per capability rather than one per attempt.
	if _, err := b.SnapshotObject(ctx, "o"); !errors.Is(err, driver.ErrNotSupported) {
		t.Errorf("a narrowed capability gave %v, want the local refusal", err)
	}
	if d.calls[driver.Snapshot] != 1 {
		t.Errorf("the store was asked %d times about a capability it refused", d.calls[driver.Snapshot])
	}
}

// TestOnlyTheRefusedCapabilityNarrows keeps one denial from taking the rest of
// a backend with it.
func TestOnlyTheRefusedCapabilityNarrows(t *testing.T) {
	b, err := driver.Open(&picky{deny: map[driver.Capability]bool{driver.DeleteObject: true}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := b.DeleteObject(ctx, "o"); !errors.Is(err, driver.ErrDenied) {
		t.Fatal(err)
	}
	for _, c := range []driver.Capability{driver.ObjectSize, driver.WriteObject, driver.Snapshot, driver.Clone} {
		if !b.Supports(c) {
			t.Errorf("%s stopped being offered because delete was refused", c)
		}
	}
	if got := b.Denied(); len(got) != 1 || got[0] != driver.DeleteObject {
		t.Errorf("denied = %v, want only delete", got)
	}
}

// TestTwoDenialsAreBothRemembered covers the copy-on-write, where a second
// narrowing that dropped the first would let a capability come back.
func TestTwoDenialsAreBothRemembered(t *testing.T) {
	b, err := driver.Open(&picky{deny: map[driver.Capability]bool{driver.Snapshot: true, driver.Clone: true}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	b.SnapshotObject(ctx, "o")
	b.CloneObject(ctx, "a", "b")

	if b.Supports(driver.Snapshot) || b.Supports(driver.Clone) {
		t.Error("a capability came back after a second was refused")
	}
	if got := b.Denied(); len(got) != 2 {
		t.Errorf("denied = %v, want both", got)
	}
}

// TestAnOrdinaryFailureDoesNotNarrow matters because a store that could not
// answer this time will answer next time, and treating that as a permission
// would remove a capability the credential has.
func TestAnOrdinaryFailureDoesNotNarrow(t *testing.T) {
	b, err := driver.Open(&picky{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.SizeOf(context.Background(), "o"); err != nil {
		t.Fatalf("an allowed call failed: %v", err)
	}
	if !b.Supports(driver.ObjectSize) {
		t.Error("a capability narrowed on a call that succeeded")
	}
	if got := b.Denied(); len(got) != 0 {
		t.Errorf("denied = %v with nothing refused", got)
	}
}

// TestReadRangeIsNeverNarrowed covers the mandatory core. A credential that
// cannot read is a broken configuration rather than a narrower store, and a
// backend that withdrew it would be one driver.Open would have refused.
func TestReadRangeIsNeverNarrowed(t *testing.T) {
	b, err := driver.Open(&picky{deny: map[driver.Capability]bool{driver.ReadRange: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadRange(context.Background(), "o", 0, 1); !errors.Is(err, driver.ErrDenied) {
		t.Fatalf("the refusal did not reach the caller: %v", err)
	}
	if !b.Supports(driver.ReadRange) {
		t.Error("the mandatory core was withdrawn")
	}
	if got := b.Denied(); len(got) != 0 {
		t.Errorf("denied = %v, want the core left alone", got)
	}
}

// listing is a driver that holds keys and derives levels the way an object
// store does, so a test can drive the backend without either real driver.
type listing struct {
	picky
	keys map[string]int64
}

func (l *listing) Declare() driver.Declaration {
	return driver.Declaration{Contract: 1, Capabilities: []driver.Capability{
		driver.ReadRange, driver.ListObjects,
	}}
}

func (l *listing) List(_ context.Context, prefix, after string, limit int) ([]driver.Entry, error) {
	full := prefix
	if full != "" && !strings.HasSuffix(full, "/") {
		full += "/"
	}
	seen := map[string]driver.Entry{}
	for k, size := range l.keys {
		if !strings.HasPrefix(k, full) {
			continue
		}
		rest := strings.TrimPrefix(k, full)
		if i := strings.Index(rest, "/"); i >= 0 {
			seen[rest[:i]] = driver.Entry{Name: rest[:i], Dir: true}
			continue
		}
		seen[rest] = driver.Entry{Name: rest, Bytes: size}
	}
	out := make([]driver.Entry, 0, len(seen))
	for _, e := range seen {
		if e.Name > after {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// TestADeclaredListingMustBeImplemented is the one lie Open can catch for
// free, and catching it means a namespace never half-works: an export would
// otherwise list nothing and give no reason.
func TestADeclaredListingMustBeImplemented(t *testing.T) {
	// picky declares everything and implements no Lister.
	claimant := &picky{}
	decl := claimant.Declare()
	decl.Capabilities = append(decl.Capabilities, driver.ListObjects)

	if _, err := driver.Open(&declaring{picky: *claimant, decl: decl}); err == nil {
		t.Error("a driver declaring listing without implementing it was opened")
	}
}

// declaring is a driver whose declaration a test chooses.
type declaring struct {
	picky
	decl driver.Declaration
}

func (d *declaring) Declare() driver.Declaration { return d.decl }

// TestALevelIsOneLevel is the shape both drivers have to answer alike: a
// filesystem has directories and an object store derives them, and a namespace
// built on one has to work on the other.
func TestALevelIsOneLevel(t *testing.T) {
	b, err := driver.Open(&listing{keys: map[string]int64{
		"imagenet/v17/shard-0": 10,
		"imagenet/v17/shard-1": 20,
		"imagenet/v18/shard-0": 30,
		"captions/v3/shard-0":  40,
		"loose-object":         50,
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	root, err := b.List(ctx, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []driver.Entry{
		{Name: "captions", Dir: true},
		{Name: "imagenet", Dir: true},
		{Name: "loose-object", Bytes: 50},
	}
	if len(root) != len(want) {
		t.Fatalf("the root lists %+v, want three names", root)
	}
	for i := range want {
		if root[i] != want[i] {
			t.Errorf("root[%d] = %+v, want %+v", i, root[i], want[i])
		}
	}

	// One level down, and nothing from further down it.
	versions, err := b.List(ctx, "imagenet", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || !versions[0].Dir || versions[0].Name != "v17" {
		t.Errorf("imagenet lists %+v, want its two versions as directories", versions)
	}
}

// TestAListingPagesWithoutRepeating matters because a caller pages on the last
// name it saw, and a listing that returned it again would loop.
func TestAListingPagesWithoutRepeating(t *testing.T) {
	b, err := driver.Open(&listing{keys: map[string]int64{
		"a": 1, "b": 2, "c": 3, "d": 4,
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var seen []string
	after := ""
	for i := 0; i < 10; i++ {
		page, err := b.List(ctx, "", after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, e := range page {
			seen = append(seen, e.Name)
		}
		after = page[len(page)-1].Name
	}
	if got := strings.Join(seen, ""); got != "abcd" {
		t.Errorf("paging saw %q, want each name once in order", got)
	}
}

// TestListingIsRefusedWhenNotDeclared keeps an export from telling a client a
// dataset is empty when the truth is that this backend cannot say.
func TestListingIsRefusedWhenNotDeclared(t *testing.T) {
	b, err := driver.Open(&picky{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.List(context.Background(), "", "", 10); !errors.Is(err, driver.ErrNotSupported) {
		t.Errorf("listing an undeclared backend gave %v", err)
	}
}

// TestAListingNeedsALimit covers the caller that meant everything: a prefix
// may hold millions, and unbounded is not a size.
func TestAListingNeedsALimit(t *testing.T) {
	b, err := driver.Open(&listing{keys: map[string]int64{"a": 1}})
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{0, -1} {
		if _, err := b.List(context.Background(), "", "", limit); err == nil {
			t.Errorf("a listing of %d names was accepted", limit)
		}
	}
}
