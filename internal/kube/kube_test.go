package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// apiServer stands in for the real one, recording what it was asked.
type apiServer struct {
	datasets []Dataset
	patched  map[string]DatasetStatus
	paths    []string
	fail     int
	body     string
}

func (a *apiServer) handler(t *testing.T) http.Handler {
	t.Helper()
	if a.patched == nil {
		a.patched = map[string]DatasetStatus{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.paths = append(a.paths, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("request carried %q, want the bearer token", got)
		}
		if a.fail != 0 {
			w.WriteHeader(a.fail)
			io := a.body
			if io == "" {
				io = `{"reason":"Forbidden","message":"datasets.forebay.io is forbidden"}`
			}
			w.Write([]byte(io))
			return
		}
		switch {
		case r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(DatasetList{Items: a.datasets})
		case r.Method == http.MethodPatch:
			if ct := r.Header.Get("Content-Type"); ct != "application/merge-patch+json" {
				t.Errorf("patched with %q, want a merge patch", ct)
			}
			var got struct {
				Status DatasetStatus `json:"status"`
			}
			json.NewDecoder(r.Body).Decode(&got)
			parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/status"), "/")
			a.patched[parts[len(parts)-1]] = got.Status
			w.Write([]byte(`{}`))
		}
	})
}

func dial(t *testing.T, a *apiServer) *Client {
	t.Helper()
	srv := httptest.NewServer(a.handler(t))
	t.Cleanup(srv.Close)
	c, err := New(Config{Host: srv.URL, Token: "secret", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c.http = srv.Client()
	return c
}

// TestListAndPatchAddressTheRightObject covers the URL, since a controller
// that patched the wrong path would report success having written nothing.
func TestListAndPatchAddressTheRightObject(t *testing.T) {
	api := &apiServer{datasets: []Dataset{{Metadata: Metadata{Name: "shards", Namespace: "team"}}}}
	c := dial(t, api)
	r := DatasetResource
	r.Namespace = "team"

	var list DatasetList
	if err := c.List(context.Background(), r, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Metadata.Name != "shards" {
		t.Fatalf("listed %+v", list.Items)
	}
	if err := c.PatchStatus(context.Background(), r, "shards", DatasetStatus{Present: true, Bytes: 42}); err != nil {
		t.Fatal(err)
	}
	if got := api.patched["shards"]; !got.Present || got.Bytes != 42 {
		t.Errorf("patched %+v", got)
	}
	want := []string{
		"GET /apis/forebay.io/v1alpha1/namespaces/team/datasets",
		"PATCH /apis/forebay.io/v1alpha1/namespaces/team/datasets/shards/status",
	}
	for i, w := range want {
		if i >= len(api.paths) || api.paths[i] != w {
			t.Errorf("request %d was %q, want %q", i, api.paths[i], w)
		}
	}
}

// TestAllNamespacesOmitsTheSegment covers the cluster-wide form, which a
// controller watching every namespace needs.
func TestAllNamespacesOmitsTheSegment(t *testing.T) {
	api := &apiServer{}
	c := dial(t, api)
	if err := c.List(context.Background(), DatasetResource, &DatasetList{}); err != nil {
		t.Fatal(err)
	}
	if got := api.paths[0]; got != "GET /apis/forebay.io/v1alpha1/datasets" {
		t.Errorf("listed %q, want no namespace segment", got)
	}
}

// TestARefusalCarriesTheServersReason keeps a controller from reporting a
// number where the API server gave words, since RBAC is what this usually is.
func TestARefusalCarriesTheServersReason(t *testing.T) {
	api := &apiServer{fail: http.StatusForbidden}
	c := dial(t, api)
	err := c.List(context.Background(), DatasetResource, &DatasetList{})
	if err == nil {
		t.Fatal("a forbidden list succeeded")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("err = %v, want the server's own message", err)
	}
	var s *Status
	if !errors.As(err, &s) || s.Code != http.StatusForbidden {
		t.Errorf("err = %v, want a Status carrying 403", err)
	}
	if NotFound(err) {
		t.Error("a forbidden error was read as not found")
	}
}

// TestNotFoundIsItsOwnAnswer covers the case a controller treats as an object
// that has gone rather than as a failure to look.
func TestNotFoundIsItsOwnAnswer(t *testing.T) {
	api := &apiServer{fail: http.StatusNotFound, body: `{"reason":"NotFound","message":"datasets.forebay.io \"gone\" not found"}`}
	c := dial(t, api)
	err := c.PatchStatus(context.Background(), DatasetResource, "gone", DatasetStatus{})
	if !NotFound(err) {
		t.Errorf("err = %v, want NotFound", err)
	}
}

func TestNewRefusesWhatItCannotUse(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("a client with no API server was built")
	}
	if _, err := New(Config{Host: "https://example", CA: []byte("not a certificate")}); err == nil {
		t.Error("a client with an unparseable certificate was built")
	}
}

// TestInClusterNeedsAllOfIt covers the four ways a pod can fail to be given
// what it needs, since each produces a different fix for whoever deployed it.
func TestInClusterNeedsAllOfIt(t *testing.T) {
	dir := t.TempDir()
	token, ca := filepath.Join(dir, "token"), filepath.Join(dir, "ca.crt")
	old, oldCA := tokenPath, caPath
	tokenPath, caPath = token, ca
	t.Cleanup(func() { tokenPath, caPath = old, oldCA })

	t.Run("outside a cluster", func(t *testing.T) {
		t.Setenv("KUBERNETES_SERVICE_HOST", "")
		t.Setenv("KUBERNETES_SERVICE_PORT", "")
		if _, err := InCluster(); err == nil {
			t.Error("a config was built outside a cluster")
		}
	})

	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")

	t.Run("with no token", func(t *testing.T) {
		if _, err := InCluster(); err == nil {
			t.Error("a config was built with no service account token")
		}
	})

	if err := os.WriteFile(token, []byte("  a-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Run("with no certificate", func(t *testing.T) {
		if _, err := InCluster(); err == nil {
			t.Error("a config was built with no cluster certificate")
		}
	})

	if err := os.WriteFile(ca, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := InCluster()
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "https://10.0.0.1:443" {
		t.Errorf("host = %q", got.Host)
	}
	if got.Token != "a-token" {
		t.Errorf("token = %q, want the surrounding space gone: a header carrying a newline is rejected", got.Token)
	}
}
