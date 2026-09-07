package csi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/csi"
	"github.com/mayur-tolexo/forebay/internal/grpcwire"
	"github.com/mayur-tolexo/forebay/internal/kube"
)

// apiServing answers for one dataset at one path, and 404s everything else.
func apiServing(t *testing.T, path string, ds *kube.Dataset) *csi.Datasets {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ds == nil || r.URL.Path != path {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{
				"kind": "Status", "code": 404, "message": "datasets.forebay.io not found",
			})
			return
		}
		json.NewEncoder(w).Encode(ds)
	}))
	t.Cleanup(srv.Close)

	c, err := kube.New(kube.Config{Host: srv.URL, Token: "secret", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return csi.NewDatasets(c)
}

// datasetPath is where the client will look for a dataset, which is worth
// asserting on: a resolver that read the wrong URL would 404 every dataset
// and blame the user for a typo.
const datasetPath = "/apis/forebay.io/v1alpha1/namespaces/team/datasets/imagenet"

func TestAPresentDatasetResolvesToItsObject(t *testing.T) {
	d := apiServing(t, datasetPath, &kube.Dataset{
		Metadata: kube.Metadata{Name: "imagenet", Namespace: "team"},
		Spec:     kube.DatasetSpec{Object: "imagenet/v17"},
		Status:   &kube.DatasetStatus{Present: true, Bytes: 12 << 20},
	})
	object, bytes, err := d.Resolve(t.Context(), "team", "imagenet")
	if err != nil {
		t.Fatal(err)
	}
	if object != "imagenet/v17" || bytes != 12<<20 {
		t.Errorf("resolved to %q %d", object, bytes)
	}
}

func TestADatasetThatIsNotDeclaredIsNotFound(t *testing.T) {
	d := apiServing(t, datasetPath, nil)
	_, _, err := d.Resolve(t.Context(), "team", "imagenet")
	if grpcwire.CodeOf(err) != grpcwire.NotFound {
		t.Errorf("got %v from %v", grpcwire.CodeOf(err), err)
	}
}

func TestADatasetNothingHasReconciledIsAWait(t *testing.T) {
	// Unavailable rather than a refusal: the provisioner retries this one, and
	// the operator is on its way. Refusing would fail a claim made a moment
	// before the operator got to it.
	d := apiServing(t, datasetPath, &kube.Dataset{
		Metadata: kube.Metadata{Name: "imagenet", Namespace: "team"},
		Spec:     kube.DatasetSpec{Object: "imagenet/v17"},
	})
	_, _, err := d.Resolve(t.Context(), "team", "imagenet")
	if grpcwire.CodeOf(err) != grpcwire.Unavailable {
		t.Errorf("got %v from %v", grpcwire.CodeOf(err), err)
	}
}

func TestADatasetWhoseBytesAreNotThereIsRefused(t *testing.T) {
	// Mounting it would give a pod an empty directory with no way to tell
	// that from a dataset that is genuinely empty, and the job would read
	// nothing and carry on.
	d := apiServing(t, datasetPath, &kube.Dataset{
		Metadata: kube.Metadata{Name: "imagenet", Namespace: "team"},
		Spec:     kube.DatasetSpec{Object: "imagenet/v17"},
		Status:   &kube.DatasetStatus{Present: false, Reason: "the store answered 403"},
	})
	_, _, err := d.Resolve(t.Context(), "team", "imagenet")
	if grpcwire.CodeOf(err) != grpcwire.FailedPrecondition {
		t.Fatalf("got %v from %v", grpcwire.CodeOf(err), err)
	}
	// The store's own words, so an operator is not left guessing between
	// absent and unreachable.
	if !strings.Contains(err.Error(), "the store answered 403") {
		t.Errorf("the reason was dropped: %v", err)
	}
}

func TestADatasetNamingNoObjectIsRefused(t *testing.T) {
	d := apiServing(t, datasetPath, &kube.Dataset{
		Metadata: kube.Metadata{Name: "imagenet", Namespace: "team"},
		Status:   &kube.DatasetStatus{Present: true, Bytes: 1},
	})
	_, _, err := d.Resolve(t.Context(), "team", "imagenet")
	if grpcwire.CodeOf(err) != grpcwire.FailedPrecondition {
		t.Errorf("got %v from %v", grpcwire.CodeOf(err), err)
	}
}

func TestAnUnreachableAPIServerIsRetryable(t *testing.T) {
	// Unavailable is the code the provisioner retries. Anything else and a
	// claim fails permanently because the API server was briefly away.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c, err := kube.New(kube.Config{Host: srv.URL, Token: "secret", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = csi.NewDatasets(c).Resolve(t.Context(), "team", "imagenet")
	if grpcwire.CodeOf(err) != grpcwire.Unavailable {
		t.Errorf("got %v from %v", grpcwire.CodeOf(err), err)
	}
}
