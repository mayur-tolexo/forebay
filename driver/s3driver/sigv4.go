package s3driver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// emptyPayload is the SHA-256 of no bytes, sent on every request that has no
// body. S3 requires the header even then.
const emptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

const (
	algorithm  = "AWS4-HMAC-SHA256"
	terminator = "aws4_request"
	service    = "s3"
)

// credentials are what signing needs. Kept separate from the driver so the
// signer can be tested against published vectors without a backend.
type credentials struct {
	accessKey string
	secretKey string
	region    string
}

// sign adds the headers that authenticate a request, in place.
//
// payloadHash is hex SHA-256 of the body, which S3 signs rather than trusts:
// the header is part of the signature, so a body swapped in flight fails to
// verify instead of being read.
func sign(r *http.Request, c credentials, payloadHash string, now time.Time) {
	stamp := now.UTC().Format("20060102T150405Z")
	day := stamp[:8]

	r.Header.Set("x-amz-date", stamp)
	r.Header.Set("x-amz-content-sha256", payloadHash)

	// RawQuery goes in unsorted, which holds only while no request here has
	// one. A signed query must be sorted and encoded first.
	signed, canonHeaders := canonicalHeaders(r)
	canonical := strings.Join([]string{
		r.Method,
		canonicalURI(r.URL.Path),
		r.URL.RawQuery,
		canonHeaders,
		signed,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{day, c.region, service, terminator}, "/")
	toSign := strings.Join([]string{
		algorithm,
		stamp,
		scope,
		hex.EncodeToString(hash([]byte(canonical))),
	}, "\n")

	key := signingKey(c.secretKey, day, c.region)
	sig := hex.EncodeToString(hmacSHA256(key, []byte(toSign)))
	r.Header.Set("Authorization", algorithm+
		" Credential="+c.accessKey+"/"+scope+
		", SignedHeaders="+signed+
		", Signature="+sig)
}

// canonicalHeaders returns the signed header list and the block that goes with
// it. Host comes from the request rather than the header map, where net/http
// keeps it.
func canonicalHeaders(r *http.Request) (string, string) {
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	values := map[string]string{"host": host}
	for name, vs := range r.Header {
		lower := strings.ToLower(name)
		if lower == "host" || lower == "authorization" {
			continue
		}
		values[lower] = strings.TrimSpace(strings.Join(vs, ","))
	}

	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteByte(':')
		b.WriteString(values[n])
		b.WriteByte('\n')
	}
	return strings.Join(names, ";"), b.String()
}

// canonicalURI re-encodes a path the way S3 signs it: every byte outside the
// unreserved set escaped, and the separators left alone.
//
// Given the decoded path, so the escaping below is the only one applied. Doing
// it over an already-escaped path would encode the percent signs again, and a
// key with a space would sign differently than it was sent.
func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = escapePath(seg)
	}
	return strings.Join(segments, "/")
}

// escapePath percent-encodes one path segment under the S3 rule.
func escapePath(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'A' && ch <= 'Z', ch >= 'a' && ch <= 'z',
			ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		default:
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{ch})))
		}
	}
	return b.String()
}

// signingKey derives the per-day, per-region key. The chain is what keeps a
// leaked signature from being usable elsewhere or later.
func signingKey(secret, day, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(day))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte(service))
	return hmacSHA256(k, []byte(terminator))
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func hash(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}
