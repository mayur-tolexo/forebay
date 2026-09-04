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
	current := &kube.DatasetStatus{Present: true, Bytes: 100, ObservedGeneration: 1}
	a := &api{items: []kube.Dataset{
		dataset("fresh", "a", nil),
		dataset("known", "b", current),
	}}
	c := a.client(t)
	s := store{sizes: map[string]int64{"a": 42, "b": 100}}

	wrote, err := reconcile(context.Background(), c, kube.DatasetResource, s)
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
	wrote, err := reconcile(context.Background(), a.client(t), kube.DatasetResource, store{sizes: map[string]int64{"a": 1}})
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
	wrote, err := reconcile(context.Background(), a.client(t), kube.DatasetResource, store{sizes: map[string]int64{"a": 7}})
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
	if _, err := reconcile(context.Background(), a.client(t), kube.DatasetResource, store{}); err == nil {
		t.Fatal("a forbidden list was treated as an empty cluster")
	}
}

// TestAnUnreachableStoreIsRecordedRatherThanDropped covers the state an
// operator most needs to see, since it is theirs to fix.
func TestAnUnreachableStoreIsRecordedRatherThanDropped(t *testing.T) {
	a := &api{items: []kube.Dataset{dataset("shards", "a", nil)}}
	wrote, err := reconcile(context.Background(), a.client(t), kube.DatasetResource,
		store{fail: errors.New("dial tcp: no route to host")})
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
		store{sizes: map[string]int64{"a": 1, "b": 2}})
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
