package main

import (
	"strings"
	"testing"
)

// TestOpenBackendRefusesAnAmbiguousChoice keeps a node from reading whichever
// backend the code happened to check first, which nothing in a client's answer
// would reveal.
func TestOpenBackendRefusesAnAmbiguousChoice(t *testing.T) {
	t.Setenv(accessKeyEnv, "k")
	t.Setenv(secretKeyEnv, "s")
	_, err := openBackend(backendOptions{
		Dir: t.TempDir(), Endpoint: "http://example.com", Bucket: "b",
	})
	if err == nil {
		t.Fatal("two backends were accepted")
	}
	if !strings.Contains(err.Error(), "pass one") {
		t.Errorf("err = %v, want it to say to pass one", err)
	}
}

func TestOpenBackendNeedsOne(t *testing.T) {
	if _, err := openBackend(backendOptions{}); err != errNoBackend {
		t.Errorf("err = %v, want errNoBackend", err)
	}
}

// TestS3BackendNeedsItsCredentials checks the failure names where they come
// from, since they are deliberately not flags.
func TestS3BackendNeedsItsCredentials(t *testing.T) {
	t.Setenv(accessKeyEnv, "")
	t.Setenv(secretKeyEnv, "")
	_, err := openBackend(backendOptions{Endpoint: "http://example.com", Bucket: "b"})
	if err == nil {
		t.Fatal("an S3 backend opened with no credentials")
	}
	if !strings.Contains(err.Error(), accessKeyEnv) {
		t.Errorf("err = %v, want it to name %s", err, accessKeyEnv)
	}
}

// TestS3BackendNeedsBothHalves keeps an endpoint with no bucket from opening
// as if it were configured.
func TestS3BackendNeedsBothHalves(t *testing.T) {
	t.Setenv(accessKeyEnv, "k")
	t.Setenv(secretKeyEnv, "s")
	if _, err := openBackend(backendOptions{Endpoint: "http://example.com"}); err == nil {
		t.Error("an endpoint with no bucket was accepted")
	}
	if _, err := openBackend(backendOptions{Bucket: "b"}); err == nil {
		t.Error("a bucket with no endpoint was accepted")
	}
}

// TestS3BackendOpens covers the case the flags exist for.
func TestS3BackendOpens(t *testing.T) {
	t.Setenv(accessKeyEnv, "k")
	t.Setenv(secretKeyEnv, "s")
	b, err := openBackend(backendOptions{Endpoint: "http://example.com", Bucket: "b"})
	if err != nil {
		t.Fatalf("opening = %v", err)
	}
	if !b.Supports("read-range") {
		t.Error("the backend does not declare read-range")
	}
}

// TestScopeSeparatesBackends checks the name the fast tier keys on, since two
// backends sharing one would answer for each other's objects.
func TestScopeSeparatesBackends(t *testing.T) {
	dir, err := backendOptions{Dir: t.TempDir()}.scope()
	if err != nil {
		t.Fatal(err)
	}
	one, err := backendOptions{Endpoint: "http://a", Bucket: "b"}.scope()
	if err != nil {
		t.Fatal(err)
	}
	two, err := backendOptions{Endpoint: "http://a", Bucket: "other"}.scope()
	if err != nil {
		t.Fatal(err)
	}
	if one == two || one == dir || two == dir {
		t.Errorf("scopes collide: %q %q %q", dir, one, two)
	}
}

// TestDescribeNamesTheBackend checks the startup line identifies which store
// is being read. Credentials cannot appear in it: backendOptions does not hold
// them, which is why they are read from the environment at the point of use.
func TestDescribeNamesTheBackend(t *testing.T) {
	got := describe(backendOptions{Endpoint: "http://example.com", Bucket: "shards"})
	if !strings.Contains(got, "example.com") || !strings.Contains(got, "shards") {
		t.Errorf("describe = %q, want the endpoint and bucket", got)
	}
	if got := describe(backendOptions{Dir: "/data"}); !strings.Contains(got, "/data") {
		t.Errorf("describe = %q, want the directory", got)
	}
}
