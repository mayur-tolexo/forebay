package s3driver

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSignMatchesPublishedVector checks the signer against AWS's own worked
// example for a ranged GET, which is the request this driver makes most.
//
// A signer can only be wrong or exactly right, and every other test here would
// pass against a consistent but incorrect one, since they check it against
// itself.
func TestSignMatchesPublishedVector(t *testing.T) {
	r, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Range", "bytes=0-9")

	creds := credentials{
		accessKey: "AKIAIOSFODNN7EXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		region:    "us-east-1",
	}
	when := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	sign(r, creds, emptyPayload, when)

	const want = "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	got := r.Header.Get("Authorization")
	if !strings.HasSuffix(got, "Signature="+want) {
		t.Errorf("Authorization = %q\nwant signature %s", got, want)
	}
	if h := r.Header.Get("x-amz-date"); h != "20130524T000000Z" {
		t.Errorf("x-amz-date = %q", h)
	}
}

// TestCanonicalURIEscapes covers the keys that separate a correct signer from
// one that works until a name has a space in it.
func TestCanonicalURIEscapes(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/bucket/plain.txt", "/bucket/plain.txt"},
		{"/bucket/with space", "/bucket/with%20space"},
		{"/bucket/a+b", "/bucket/a%2Bb"},
		{"/bucket/nested/key", "/bucket/nested/key"},
		{"/bucket/tilde~ok", "/bucket/tilde~ok"},
		{"", "/"},
	} {
		if got := canonicalURI(c.in); got != c.want {
			t.Errorf("canonicalURI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
