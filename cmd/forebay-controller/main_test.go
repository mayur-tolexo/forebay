package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/driver"
	"github.com/mayur-tolexo/forebay/internal/intent"
	"github.com/mayur-tolexo/forebay/internal/kube"
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
		return 0, errors.New("no such key")
	}
	return n, nil
}

// api stands in for the server, counting the writes a pass made.
type api struct {
	items   []kube.Dataset
	patches map[string]kube.DatasetStatus
	gone    map[string]bool
	refuse  map[string]bool
	listErr int
}

func (a *api) client(t *testing.T) *kube.Client {
	t.Helper()
	a.patches = map[string]kube.DatasetStatus{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if a.listErr != 0 {
				w.WriteHeader(a.listErr)
				w.Write([]byte(`{"reason":"Forbidden"}`))
				return
			}
			json.NewEncoder(w).Encode(kube.DatasetList{Items: a.items})
			return
		}
		parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/status"), "/")
		name := parts[len(parts)-1]
		if a.gone[name] {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"reason":"NotFound"}`))
			return
		}
		if a.refuse[name] {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"reason":"Conflict","message":"the object was modified"}`))
			return
		}
		var got struct {
			Status kube.DatasetStatus `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&got)
		a.patches[name] = got.Status
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c, err := kube.New(kube.Config{Host: srv.URL, Token: "t", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// resolvable is a backend and a fleet that can meet the default intent, so a
// test about reconciling is not also a test about intent.
func resolvable(t *testing.T) kube.Resolvable {
	t.Helper()
	b, err := driver.Open(plain{})
	if err != nil {
		t.Fatal(err)
	}
	return kube.Resolvable{Backend: b, Fleet: intent.Fleet{KnowsRacks: true}}
}

// plain declares the mandatory core and nothing else.
type plain struct{}

func (plain) Declare() driver.Declaration {
	return driver.Declaration{Contract: 1, Capabilities: []driver.Capability{driver.ReadRange}}
}
func (plain) ReadRange(context.Context, string, int64, int64) ([]byte, error) { return nil, nil }
func (plain) SizeOf(context.Context, string) (int64, error)                   { return 0, nil }
func (plain) WriteObject(context.Context, string, []byte) error               { return nil }
func (plain) DeleteObject(context.Context, string) error                      { return nil }
func (plain) SnapshotObject(context.Context, string) (string, error)          { return "", nil }
func (plain) CloneObject(context.Context, string, string) error               { return nil }

func dataset(name, object string, status *kube.DatasetStatus) kube.Dataset {
	return kube.Dataset{
		Metadata: kube.Metadata{Name: name, Namespace: "team", Generation: 1},
		Spec:     kube.DatasetSpec{Object: object},
		Status:   status,
	}
}

// TestReconcileWritesOnlyWhatChanged is the property that keeps a controller
// off etcd's back: every pass resolves every dataset, and almost every pass
// should write nothing.
func TestReconcileWritesOnlyWhatChanged(t *testing.T) {
	// Satisfiable is part of what is recorded, so a status written before this
	// field existed differs from a freshly resolved one and is rewritten once.
	// That is a real upgrade cost and it is one write per dataset, not one per
	// pass.
	current := &kube.DatasetStatus{Present: true, Bytes: 100, ObservedGeneration: 1, Satisfiable: true}
	a := &api{items: []kube.Dataset{
		dataset("fresh", "a", nil),
		dataset("known", "b", current),
	}}
	c := a.client(t)
	s := store{sizes: map[string]int64{"a": 42, "b": 100}}

	wrote, err := reconcile(context.Background(), c, kube.DatasetResource,
		s, resolvable(t))
	if err != nil {
		t.Fatal(err)
	}
	if wrote != 1 {
		t.Errorf("wrote %d, want only the one that changed", wrote)
	}
	if _, ok := a.patches["known"]; ok {
		t.Error("a dataset whose status was already right was written again")
	}
	if got := a.patches["fresh"]; !got.Present || got.Bytes != 42 {
		t.Errorf("fresh = %+v", got)
	}
}

// TestADatasetDeletedMidPassIsNotAFailure covers the ordinary race between
// listing and writing.
func TestADatasetDeletedMidPassIsNotAFailure(t *testing.T) {
	a := &api{items: []kube.Dataset{dataset("going", "a", nil)}, gone: map[string]bool{"going": true}}
	wrote, err := reconcile(context.Background(), a.client(t), kube.DatasetResource, store{sizes: map[string]int64{"a": 1}}, resolvable(t))
	if err != nil {
		t.Fatalf("a deleted dataset failed the pass: %v", err)
	}
	if wrote != 0 {
		t.Errorf("wrote %d for an object that had gone", wrote)
	}
}

// TestOneBadDatasetDoesNotStopTheRest keeps a single malformed declaration
// from leaving every other dataset unresolved.
func TestOneBadDatasetDoesNotStopTheRest(t *testing.T) {
	a := &api{items: []kube.Dataset{
		dataset("empty", "", nil),
		dataset("good", "a", nil),
	}}
	wrote, err := reconcile(context.Background(), a.client(t), kube.DatasetResource, store{sizes: map[string]int64{"a": 7}}, resolvable(t))
	if err != nil {
		t.Fatal(err)
	}
	if wrote != 1 {
		t.Errorf("wrote %d, want the good one", wrote)
	}
	if _, ok := a.patches["empty"]; ok {
		t.Error("a dataset naming no object was written")
	}
}

// TestAListThatFailedIsReported keeps a pass that saw nothing from being
// reported as a cluster with no datasets.
func TestAListThatFailedIsReported(t *testing.T) {
	a := &api{listErr: http.StatusForbidden}
	if _, err := reconcile(context.Background(), a.client(t), kube.DatasetResource,
		store{}, resolvable(t)); err == nil {
		t.Fatal("a forbidden list was treated as an empty cluster")
	}
}

// TestAnUnreachableStoreIsRecordedRatherThanDropped covers the state an
// operator most needs to see, since it is theirs to fix.
func TestAnUnreachableStoreIsRecordedRatherThanDropped(t *testing.T) {
	a := &api{items: []kube.Dataset{dataset("shards", "a", nil)}}
	wrote, err := reconcile(context.Background(), a.client(t), kube.DatasetResource,
		store{fail: errors.New("dial tcp: no route to host")}, resolvable(t))
	if err != nil {
		t.Fatal(err)
	}
	if wrote != 1 {
		t.Fatalf("wrote %d, want the failure recorded", wrote)
	}
	got := a.patches["shards"]
	if got.Present {
		t.Error("an unreachable store was recorded as present")
	}
	if !strings.Contains(got.Reason, "no route to host") {
		t.Errorf("reason = %q, want the store's own words", got.Reason)
	}
}

// TestOpenBackendNeedsCredentials keeps the controller from starting against a
// store it cannot read, since every pass would then record every dataset as
// unreachable and the reason would point at the store rather than at the
// deployment that forgot them.
func TestOpenBackendNeedsCredentials(t *testing.T) {
	t.Setenv(accessKeyEnv, "")
	t.Setenv(secretKeyEnv, "")
	if _, err := openBackend("http://store", "bucket", ""); err == nil {
		t.Error("a backend opened with no credentials")
	}

	t.Setenv(accessKeyEnv, "key")
	t.Setenv(secretKeyEnv, "secret")
	b, err := openBackend("http://store", "bucket", "us-east-1")
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	if !b.Supports("object-size") {
		t.Error("the backend cannot answer how large an object is, which is all resolving needs")
	}
}

// TestOpenClientPrefersTheFlags covers running outside a cluster, which is how
// this is driven against a kubeconfig while it is being written.
func TestOpenClientPrefersTheFlags(t *testing.T) {
	if _, err := openClient("https://api.example", "token"); err != nil {
		t.Errorf("a client from flags was refused: %v", err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if _, err := openClient("", ""); err == nil {
		t.Error("a client was built with neither flags nor a pod's credentials")
	}
}

func TestCheckFlags(t *testing.T) {
	if err := checkFlags("http://store", "bucket", time.Second); err != nil {
		t.Fatalf("a usable configuration was refused: %v", err)
	}
	for _, c := range []struct {
		name string
		err  error
	}{
		{"no endpoint", checkFlags("", "bucket", time.Second)},
		{"no bucket", checkFlags("http://store", "", time.Second)},
		{"no interval", checkFlags("http://store", "bucket", 0)},
		{"a negative interval", checkFlags("http://store", "bucket", -time.Second)},
	} {
		if c.err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

// TestOneUnwritableDatasetDoesNotStopTheRest covers a pass that would
// otherwise leave every object after a broken one unresolved for as long as it
// stayed broken.
func TestOneUnwritableDatasetDoesNotStopTheRest(t *testing.T) {
	a := &api{items: []kube.Dataset{
		dataset("refused", "a", nil),
		dataset("fine", "b", nil),
	}, refuse: map[string]bool{"refused": true}}

	wrote, err := reconcile(context.Background(), a.client(t), kube.DatasetResource,
		store{sizes: map[string]int64{"a": 1, "b": 2}}, resolvable(t))
	if err == nil {
		t.Fatal("a refused write was not reported")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("err = %v, want it to name the object", err)
	}
	if wrote != 1 {
		t.Errorf("wrote %d, want the one that came after the failure", wrote)
	}
	if _, ok := a.patches["fine"]; !ok {
		t.Error("the dataset after the broken one was never reconciled")
	}
}

// TestAnUnsatisfiableIntentIsRecordedAndKeepsItsData covers a declaration the
// system cannot honour. The dataset is not deleted and its object is not
// touched: what changed is the ability to meet the intent, not the request.
func TestAnUnsatisfiableIntentIsRecordedAndKeepsItsData(t *testing.T) {
	d := dataset("wants-racks", "a", nil)
	d.Spec.Intent = intent.Intent{Durability: intent.DurabilityRackTolerant}
	a := &api{items: []kube.Dataset{d}}

	// A backend that can replicate, on a fleet that cannot name a rack.
	b, err := driver.Open(replicating{})
	if err != nil {
		t.Fatal(err)
	}
	r := kube.Resolvable{Backend: b, Fleet: intent.Fleet{KnowsRacks: false}}

	if _, err := reconcile(context.Background(), a.client(t), kube.DatasetResource,
		store{sizes: map[string]int64{"a": 64}}, r); err != nil {
		t.Fatal(err)
	}
	got := a.patches["wants-racks"]
	if got.Satisfiable {
		t.Error("an intent this fleet cannot meet was recorded as satisfiable")
	}
	if !strings.Contains(got.Unsatisfiable, "rack") {
		t.Errorf("the reason does not name the fleet's own blindness: %q", got.Unsatisfiable)
	}
	if !got.Present || got.Bytes != 64 {
		t.Errorf("the object was forgotten because its intent could not be met: %+v", got)
	}
}

// replicating declares replication and nothing about racks, which no backend
// can declare.
type replicating struct{ plain }

func (replicating) Declare() driver.Declaration {
	return driver.Declaration{Contract: 1, Capabilities: []driver.Capability{driver.ReadRange, driver.Replicate}}
}
