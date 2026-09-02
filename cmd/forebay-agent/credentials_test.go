package main

import (
	"strings"
	"testing"
)

const embedded = "http://AKIAEXAMPLE:supersecret@store.example.com:8080"

// TestEndpointWithCredentialsIsRefused covers the habit of writing them into
// the URL. Taking it would put the secret in the startup line and in the
// tier's keys, which reading them from the environment exists to avoid.
func TestEndpointWithCredentialsIsRefused(t *testing.T) {
	t.Setenv(accessKeyEnv, "k")
	t.Setenv(secretKeyEnv, "s")
	_, err := openBackend(backendOptions{Endpoint: embedded, Bucket: "b"})
	if err == nil {
		t.Fatal("an endpoint carrying credentials was accepted")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Errorf("the refusal repeated the secret: %v", err)
	}
	if !strings.Contains(err.Error(), accessKeyEnv) {
		t.Errorf("err = %v, want it to say where credentials go", err)
	}
}

// TestNamesDropAnythingInFrontOfTheHost keeps the two names derived from an
// endpoint clean even if one reaches them.
func TestNamesDropAnythingInFrontOfTheHost(t *testing.T) {
	o := backendOptions{Endpoint: embedded, Bucket: "b"}
	if got := describe(o); strings.Contains(got, "supersecret") {
		t.Errorf("describe = %q, want no credential", got)
	}
	got, err := o.scope()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "supersecret") {
		t.Errorf("scope = %q, want no credential", got)
	}
	if strings.Contains(got, "http://") {
		t.Errorf("scope = %q, want one scheme", got)
	}
}

// TestScopeIgnoresTheScheme keeps the same bucket on one host from caching
// twice, since http and https serve the same objects.
func TestScopeIgnoresTheScheme(t *testing.T) {
	plain, err := backendOptions{Endpoint: "http://store:8080", Bucket: "b"}.scope()
	if err != nil {
		t.Fatal(err)
	}
	secure, err := backendOptions{Endpoint: "https://store:8080", Bucket: "b"}.scope()
	if err != nil {
		t.Fatal(err)
	}
	if plain != secure {
		t.Errorf("scopes differ by scheme: %q and %q", plain, secure)
	}
}
