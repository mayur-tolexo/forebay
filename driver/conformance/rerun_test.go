package conformance_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/mayur-tolexo/forebay/driver"
	"github.com/mayur-tolexo/forebay/driver/conformance"
)

// worm writes once and cannot delete. Immutability makes that a realistic
// backend rather than a contrived one.
type worm struct {
	objects map[string][]byte
	// dishonest makes it perform a capability it did not declare.
	dishonest bool
}

func (w *worm) Declare() driver.Declaration {
	return driver.Declaration{Contract: 1, Capabilities: []driver.Capability{driver.ReadRange, driver.WriteObject}}
}

// SizeOf refuses unless declared, which this driver does not declare: a store
// that writes once still need not be able to say how large a thing is.
func (w *worm) SizeOf(_ context.Context, object string) (int64, error) {
	if !w.dishonest {
		return 0, fmt.Errorf("%w: %s", driver.ErrNotSupported, driver.ObjectSize)
	}
	return int64(len(w.objects[object])), nil
}

func (w *worm) ReadRange(_ context.Context, o string, off, n int64) ([]byte, error) {
	b, ok := w.objects[o]
	if !ok {
		return nil, fmt.Errorf("no such object %q", o)
	}
	if off+n > int64(len(b)) {
		return nil, fmt.Errorf("%w: past the end", driver.ErrRange)
	}
	return b[off : off+n], nil
}

func (w *worm) WriteObject(_ context.Context, o string, d []byte) error {
	if _, exists := w.objects[o]; exists {
		return fmt.Errorf("object %q already exists and this store is write-once", o)
	}
	w.objects[o] = d
	return nil
}

func (w *worm) DeleteObject(_ context.Context, o string) error {
	if w.dishonest {
		delete(w.objects, o)
		return nil
	}
	return fmt.Errorf("%w: %s", driver.ErrNotSupported, driver.DeleteObject)
}
func (w *worm) SnapshotObject(context.Context, string) (string, error) {
	return "", fmt.Errorf("%w: %s", driver.ErrNotSupported, driver.Snapshot)
}
func (w *worm) CloneObject(_ context.Context, _, to string) error {
	if w.dishonest {
		w.objects[to] = []byte("copied, which is not a clone")
		return nil
	}
	return fmt.Errorf("%w: %s", driver.ErrNotSupported, driver.Clone)
}

func wormFixture(w *worm) conformance.Fixture {
	return conformance.Fixture{
		Driver: w, Object: "o", Content: []byte("abcdefgh"), WritablePrefix: "suite",
	}
}

func TestTheSuiteRunsTwiceAgainstAStoreThatCannotDelete(t *testing.T) {
	// It passed once and failed forever after, blaming the driver for an
	// object the suite itself had left behind. A third party running this
	// against a real store is the whole point, and they will run it twice.
	w := &worm{objects: map[string][]byte{"o": []byte("abcdefgh")}}
	f := wormFixture(w)
	for run := 1; run <= 3; run++ {
		if found := conformance.Check(f); len(found) != 0 {
			t.Errorf("run %d: %v", run, found)
		}
	}
}

func TestTheSuiteTidiesUpAfterADishonestDriver(t *testing.T) {
	// Catching a lie means attempting the operation, so a driver that quietly
	// performs it creates something. Leaving that in somebody's store is a
	// poor way to repay them for running the suite.
	w := &worm{objects: map[string][]byte{"o": []byte("abcdefgh")}, dishonest: true}
	if found := conformance.Check(wormFixture(w)); len(found) == 0 {
		t.Fatal("the suite passed a driver doing what it said it could not")
	}
	// Delete is undeclared, so cleanup cannot run and what remains is the
	// backend's property rather than the suite's fault. What must not happen
	// is the original object being disturbed.
	if _, ok := w.objects["o"]; !ok {
		t.Error("the suite destroyed the object it was given to read")
	}
}

func TestCleanupRemovesWhatTheSuiteCreated(t *testing.T) {
	// Where the backend can delete, the suite puts it back as it found it.
	w := &worm{objects: map[string][]byte{"o": []byte("abcdefgh")}}
	full := &deletingWorm{worm: w}
	if found := conformance.Check(conformance.Fixture{
		Driver: full, Object: "o", Content: []byte("abcdefgh"), WritablePrefix: "suite",
	}); len(found) != 0 {
		t.Fatalf("suite: %v", found)
	}
	if len(w.objects) != 1 {
		t.Errorf("backend holds %v, want only the object it started with", w.objects)
	}
}

// deletingWorm is the same store with delete declared.
type deletingWorm struct{ *worm }

func (d *deletingWorm) Declare() driver.Declaration {
	return driver.Declaration{Contract: 1, Capabilities: []driver.Capability{
		driver.ReadRange, driver.WriteObject, driver.DeleteObject,
	}}
}
func (d *deletingWorm) DeleteObject(_ context.Context, o string) error {
	delete(d.objects, o)
	return nil
}
