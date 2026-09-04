package kube

import (
	"context"
	"errors"
	"testing"

	"github.com/mayur-tolexo/forebay/driver"
	"github.com/mayur-tolexo/forebay/internal/intent"
)

// store answers about objects a test decided on.
type store struct {
	sizes map[string]int64
	fail  error
}

func (s store) SizeOf(_ context.Context, object string) (int64, error) {
	if s.fail != nil {
		return 0, s.fail
	}
	n, ok := s.sizes[object]
	if !ok {
		return 0, errors.New("s3driver: 404 Not Found: NoSuchKey")
	}
	return n, nil
}

// TestResolveTellsAbsentFromUnreachable is the distinction the status exists
// for: a dataset declared before its data is uploaded is waiting, and a store
// nobody can reach is an operator's problem, and they are not the same.
func TestResolveTellsAbsentFromUnreachable(t *testing.T) {
	d := Dataset{Metadata: Metadata{Name: "shards", Generation: 3}, Spec: DatasetSpec{Object: "shard-0"}}

	present, err := Resolve(context.Background(), store{sizes: map[string]int64{"shard-0": 1 << 20}}, d)
	if err != nil {
		t.Fatal(err)
	}
	if !present.Present || present.Bytes != 1<<20 || present.ObservedGeneration != 3 {
		t.Errorf("present = %+v", present)
	}

	absent, err := Resolve(context.Background(), store{sizes: map[string]int64{}}, d)
	if err != nil {
		t.Fatal(err)
	}
	if absent.Present || absent.Bytes != 0 {
		t.Errorf("absent = %+v, want not present", absent)
	}
	if absent.Reason == "" {
		t.Error("an absent dataset recorded no reason")
	}

	broken, err := Resolve(context.Background(), store{fail: errors.New("s3driver: dial tcp: no route to host")}, d)
	if err != nil {
		t.Fatal(err)
	}
	if broken.Present {
		t.Error("an unreachable store was reported as present")
	}
	if broken.Reason == absent.Reason {
		t.Error("unreachable and absent recorded the same reason")
	}
}

// TestResolveRefusesADatasetNamingNothing keeps an empty declaration from
// being reported as an object that is merely missing.
func TestResolveRefusesADatasetNamingNothing(t *testing.T) {
	_, err := Resolve(context.Background(), store{}, Dataset{Metadata: Metadata{Name: "empty"}})
	if !errors.Is(err, ErrNoObject) {
		t.Errorf("err = %v, want ErrNoObject", err)
	}
}

// TestChangedWritesOnlyWhatIsNew keeps a controller from patching every object
// on every pass, which on a large cluster is the difference between a watch
// and a denial of service against etcd.
func TestChangedWritesOnlyWhatIsNew(t *testing.T) {
	now := DatasetStatus{Present: true, Bytes: 100, ObservedGeneration: 2}
	if !Changed(nil, now) {
		t.Error("a status never written was called unchanged")
	}
	same := now
	if Changed(&same, now) {
		t.Error("an identical status was called changed")
	}
	for _, edit := range []func(*DatasetStatus){
		func(s *DatasetStatus) { s.Present = false },
		func(s *DatasetStatus) { s.Bytes = 101 },
		func(s *DatasetStatus) { s.ObservedGeneration = 3 },
		func(s *DatasetStatus) { s.Reason = "gone" },
	} {
		was := now
		edit(&was)
		if !Changed(&was, now) {
			t.Errorf("a difference was called unchanged: %+v against %+v", was, now)
		}
	}
}

// replicating declares a store that keeps more than one copy and says so,
// which is what a durability above the store's own asks of a backend.
type replicating struct{}

func (replicating) Declare() driver.Declaration {
	return driver.Declaration{Contract: 1, Capabilities: []driver.Capability{driver.ReadRange, driver.Replicate}}
}
func (replicating) ReadRange(context.Context, string, int64, int64) ([]byte, error) { return nil, nil }
func (replicating) SizeOf(context.Context, string) (int64, error)                   { return 0, nil }
func (replicating) WriteObject(context.Context, string, []byte) error               { return nil }
func (replicating) DeleteObject(context.Context, string) error                      { return nil }
func (replicating) SnapshotObject(context.Context, string) (string, error)          { return "", nil }
func (replicating) CloneObject(context.Context, string, string) error               { return nil }

func replicatingBackend(t *testing.T) *driver.Backend {
	t.Helper()
	b, err := driver.Open(replicating{})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAFloorRaisesADatasetAndSaysSo covers the administrator's floor. A floor
// that changed what a dataset requires without recording it would be the
// silent weakening a declarative interface exists to prevent, running the
// other way.
func TestAFloorRaisesADatasetAndSaysSo(t *testing.T) {
	r := Resolvable{Backend: replicatingBackend(t), Floor: intent.Floor{Durability: intent.DurabilityReplicated}}

	var status DatasetStatus
	ResolveIntent(Dataset{}, r, &status)
	if !status.Satisfiable {
		t.Fatalf("a dataset raised to a durability the backend offers was unsatisfiable: %s", status.Unsatisfiable)
	}
	if status.RaisedTo != string(intent.DurabilityReplicated) {
		t.Errorf("RaisedTo = %q, want the durability the floor imposed", status.RaisedTo)
	}

	// A user who already asked for it was not raised, and must not be told
	// they were.
	d := Dataset{Spec: DatasetSpec{Intent: intent.Intent{Durability: intent.DurabilityReplicated}}}
	ResolveIntent(d, r, &status)
	if status.RaisedTo != "" {
		t.Errorf("RaisedTo = %q for a user who declared it themselves", status.RaisedTo)
	}
}

// TestAFloorIsResolvedAgainstWhatWillBeEnforced matters because resolving the
// user's own declaration would record a dataset as satisfiable when the
// durability actually imposed is one the backend cannot offer.
func TestAFloorIsResolvedAgainstWhatWillBeEnforced(t *testing.T) {
	// A backend that replicates but cannot say which rack a node is in.
	r := Resolvable{
		Backend: replicatingBackend(t),
		Fleet:   intent.Fleet{KnowsRacks: false},
		Floor:   intent.Floor{Durability: intent.DurabilityRackTolerant},
	}

	var status DatasetStatus
	ResolveIntent(Dataset{}, r, &status)
	if status.Satisfiable {
		t.Error("a dataset raised past what this fleet can offer was recorded as satisfiable")
	}
}
