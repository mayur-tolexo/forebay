package kube

import (
	"context"
	"errors"
	"testing"
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
