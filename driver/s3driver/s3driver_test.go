package s3driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/mayur-tolexo/forebay/driver"
	"github.com/mayur-tolexo/forebay/driver/conformance"
)

// fakeS3 answers the way a store does, including the two behaviours the driver
// exists to cope with: an offset past the end is a 416, and a length running
// off the end is a 206 that is simply shorter.
type fakeS3 struct {
	objects map[string][]byte
	// requests counts what was actually sent, so a test can tell a driver that
	// answered from one that answered without asking.
	requests int
}

// list answers ListObjectsV2 the way a store does: keys under the prefix, and
// everything sharing the next separator collapsed into one common prefix.
//
// Modelled rather than stubbed, because the delimiter is the whole of how a
// level is derived in a store with no directories, and a fake that returned
// every key would let a driver that forgot the delimiter pass.
func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	after := q.Get("start-after")
	// Honoured rather than assumed. A store without a delimiter returns
	// every key beneath the prefix and no common prefixes at all, and a fake
	// that collapsed anyway would let a driver that forgot to ask pass.
	delimiter := q.Get("delimiter")

	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var body strings.Builder
	seen := map[string]bool{}
	body.WriteString(`<ListBucketResult>`)
	for _, k := range keys {
		rest := strings.TrimPrefix(k, prefix)
		if i := strings.Index(rest, delimiter); delimiter != "" && i >= 0 {
			p := prefix + rest[:i+1]
			if !seen[p] {
				seen[p] = true
				fmt.Fprintf(&body, `<CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes>`, p)
			}
			continue
		}
		fmt.Fprintf(&body, `<Contents><Key>%s</Key><Size>%d</Size></Contents>`,
			k, len(f.objects[k]))
	}
	body.WriteString(`</ListBucketResult>`)
	w.Write([]byte(body.String()))
}

func (f *fakeS3) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests++
		if r.Header.Get("Authorization") == "" {
			t.Errorf("unsigned %s %s", r.Method, r.URL.Path)
		}
		key := strings.TrimPrefix(r.URL.Path, "/bucket/")

		// A listing is a GET on the bucket rather than on an object, which
		// is the one request whose path is not a key.
		if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
			f.list(w, r)
			return
		}

		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("reading put body: %v", err)
			}
			// Against the body rather than merely present. A store verifies
			// this, so a driver that signs the empty hash over a real body is
			// rejected there and would pass a check that only asks if it is set.
			if got := r.Header.Get("x-amz-content-sha256"); got != sha256Hex(body) {
				t.Errorf("payload hash %s does not match the %d byte body", got, len(body))
			}
			f.objects[key] = body
			w.WriteHeader(http.StatusOK)
			return
		case http.MethodDelete:
			delete(f.objects, key)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if got := r.Header.Get("x-amz-content-sha256"); got != emptyPayload {
			t.Errorf("%s %s carried payload hash %q, want the empty one", r.Method, r.URL.Path, got)
		}

		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `<Error><Code>NoSuchKey</Code><Message>not here</Message></Error>`)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}

		first, last, ok := parseRange(r.Header.Get("Range"))
		if !ok {
			w.Write(data)
			return
		}
		if first >= len(data) {
			w.Header().Del("Content-Length")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if last >= len(data) {
			last = len(data) - 1
		}
		part := data[first : last+1]
		w.Header().Set("Content-Length", strconv.Itoa(len(part)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(part)
	})
}

// parseRange reads the one header form this driver sends.
func parseRange(h string) (int, int, bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	parts := strings.SplitN(strings.TrimPrefix(h, "bytes="), "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	first, err1 := strconv.Atoi(parts[0])
	last, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return first, last, true
}

// newFake wires a driver to a fake store holding one object.
func newFake(t *testing.T, objects map[string][]byte) (*Driver, *fakeS3) {
	t.Helper()
	fake := &fakeS3{objects: objects}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)

	d, err := New(Config{
		Endpoint:   srv.URL,
		Bucket:     "bucket",
		Region:     "us-east-1",
		AccessKey:  "key",
		SecretKey:  "secret",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, fake
}

func TestConformance(t *testing.T) {
	content := []byte("the quick brown fox jumps over the lazy dog")
	d, _ := newFake(t, map[string][]byte{"fixture": content})

	conformance.Run(t, conformance.Fixture{
		Driver:         d,
		Object:         "fixture",
		Content:        content,
		WritablePrefix: "scratch",
	})
}

// TestReadPastEndIsAnError covers the case a store answers with a short 206
// rather than a refusal, which a caller cannot tell from a truncated object.
func TestReadPastEndIsAnError(t *testing.T) {
	content := []byte("0123456789")
	d, _ := newFake(t, map[string][]byte{"o": content})

	if _, err := d.ReadRange(context.Background(), "o", 0, 11); !errors.Is(err, driver.ErrRange) {
		t.Errorf("reading 11 bytes of a 10 byte object = %v, want ErrRange", err)
	}
	if _, err := d.ReadRange(context.Background(), "o", 10, 1); !errors.Is(err, driver.ErrRange) {
		t.Errorf("reading past the end = %v, want ErrRange", err)
	}
}

// TestEmptyRangeStillChecksTheObject keeps a read of nothing from succeeding
// against a name that is not there.
func TestEmptyRangeStillChecksTheObject(t *testing.T) {
	d, fake := newFake(t, map[string][]byte{"o": []byte("abc")})
	ctx := context.Background()

	got, err := d.ReadRange(ctx, "o", 0, 0)
	if err != nil || len(got) != 0 {
		t.Errorf("empty range of a real object = %q, %v", got, err)
	}
	before := fake.requests
	if _, err := d.ReadRange(ctx, "missing", 0, 0); err == nil {
		t.Error("empty range of an absent object succeeded")
	}
	if fake.requests == before {
		t.Error("empty range answered without asking the store")
	}
}

// TestMissingObjectCarriesTheStoresReason checks the failure a operator reads.
func TestMissingObjectCarriesTheStoresReason(t *testing.T) {
	d, _ := newFake(t, map[string][]byte{})
	_, err := d.ReadRange(context.Background(), "gone", 0, 1)
	if err == nil || !strings.Contains(err.Error(), "NoSuchKey") {
		t.Errorf("error = %v, want the store's own code", err)
	}
}

// TestUndeclaredAreRefused checks the two this driver does not do answer with
// something a caller can tell from a transient failure.
func TestUndeclaredAreRefused(t *testing.T) {
	d, _ := newFake(t, map[string][]byte{})
	if _, err := d.SnapshotObject(context.Background(), "o"); !errors.Is(err, driver.ErrNotSupported) {
		t.Errorf("snapshot = %v, want ErrNotSupported", err)
	}
	if err := d.CloneObject(context.Background(), "a", "b"); !errors.Is(err, driver.ErrNotSupported) {
		t.Errorf("clone = %v, want ErrNotSupported", err)
	}
}

// TestConfigIsCheckedUpFront keeps a broken configuration from becoming a
// failed read much later, when the cause is no longer nearby.
func TestConfigIsCheckedUpFront(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  Config
	}{
		{"no endpoint", Config{Bucket: "b", AccessKey: "k", SecretKey: "s"}},
		{"endpoint without a scheme", Config{Endpoint: "example.com", Bucket: "b", AccessKey: "k", SecretKey: "s"}},
		{"no bucket", Config{Endpoint: "https://example.com", AccessKey: "k", SecretKey: "s"}},
		{"no credentials", Config{Endpoint: "https://example.com", Bucket: "b"}},
	} {
		if _, err := New(c.cfg); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

// TestBadObjectNamesAreRefused keeps a key that would address something else
// from being sent.
func TestBadObjectNamesAreRefused(t *testing.T) {
	d, fake := newFake(t, map[string][]byte{})
	for _, name := range []string{"", "/leading"} {
		if _, err := d.SizeOf(context.Background(), name); !errors.Is(err, ErrBadObject) {
			t.Errorf("SizeOf(%q) = %v, want ErrBadObject", name, err)
		}
	}
	if fake.requests != 0 {
		t.Errorf("a refused name still reached the store %d times", fake.requests)
	}
}

// refusing answers every request the way a store refuses a credential whose
// policy does not allow the call.
func refusing(t *testing.T, status int, code string) *Driver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		if code != "" {
			fmt.Fprintf(w, `<Error><Code>%s</Code><Message>policy says no</Message></Error>`, code)
		}
	}))
	t.Cleanup(srv.Close)

	d, err := New(Config{
		Endpoint: srv.URL, Bucket: "bucket", Region: "us-east-1",
		AccessKey: "key", SecretKey: "secret", HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestAForbiddenAnswerIsADenial is what makes a capability belong to the
// credential rather than to the store: without this the refusal is an error
// like any other, and a backend keeps offering something it will never do.
func TestAForbiddenAnswerIsADenial(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		code   string
	}{
		{"a bare 403", http.StatusForbidden, ""},
		{"403 with the usual code", http.StatusForbidden, "AccessDenied"},
	} {
		// Asking for a size is a HEAD, which has no body, so the status is the
		// only thing there is to go on. That is why the status is matched at
		// all rather than only the code.
		d := refusing(t, c.status, c.code)
		if _, err := d.SizeOf(context.Background(), "o"); !errors.Is(err, driver.ErrDenied) {
			t.Errorf("%s gave %v, want a denial", c.name, err)
		}
	}

	// A read has a body, so a store that says what it means in one is
	// understood even when it chose a status nobody would guess from.
	d := refusing(t, http.StatusBadRequest, "AccessDenied")
	if _, err := d.ReadRange(context.Background(), "o", 0, 1); !errors.Is(err, driver.ErrDenied) {
		t.Errorf("a coded refusal in a body gave %v, want a denial", err)
	}
}

// TestAStoreHavingABadDayIsNotADenial matters because a call that will come
// right on a retry must not remove a capability the credential has.
func TestAStoreHavingABadDayIsNotADenial(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		code   string
	}{
		{"a server error", http.StatusInternalServerError, "InternalError"},
		{"an object that is not there", http.StatusNotFound, "NoSuchKey"},
		{"a bad request", http.StatusBadRequest, "InvalidArgument"},
	} {
		d := refusing(t, c.status, c.code)
		_, err := d.ReadRange(context.Background(), "o", 0, 1)
		if err == nil {
			t.Fatalf("%s succeeded", c.name)
		}
		if errors.Is(err, driver.ErrDenied) {
			t.Errorf("%s was reported as a denial: %v", c.name, err)
		}
	}
}

// TestADenialStillSaysWhatTheStoreSaid keeps the wrapping from swallowing the
// message, which is the difference between a fixable answer and "403".
func TestADenialStillSaysWhatTheStoreSaid(t *testing.T) {
	// A read rather than a size, since a HEAD carries no body to say it in.
	d := refusing(t, http.StatusForbidden, "AccessDenied")
	_, err := d.ReadRange(context.Background(), "o", 0, 1)
	if err == nil {
		t.Fatal("the call succeeded")
	}
	for _, want := range []string{"AccessDenied", "policy says no"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not carry %q: %v", want, err)
		}
	}
}

// TestALevelIsDerivedFromKeys is what the delimiter is for. A store has no
// directories, so a version is a common prefix rather than a thing, and a
// driver that asked without one would return every shard of every version
// where a client asked for the versions.
func TestALevelIsDerivedFromKeys(t *testing.T) {
	d, _ := newFake(t, map[string][]byte{
		"imagenet/v17/shard-0": []byte("aa"),
		"imagenet/v17/shard-1": []byte("bbb"),
		"imagenet/v18/shard-0": []byte("cccc"),
		"captions/v3/shard-0":  []byte("d"),
		"loose":                []byte("ee"),
	})
	b, err := driver.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	root, err := b.List(ctx, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []driver.Entry{
		{Name: "captions", Dir: true},
		{Name: "imagenet", Dir: true},
		{Name: "loose", Bytes: 2},
	}
	if len(root) != len(want) {
		t.Fatalf("the root lists %+v, want three names", root)
	}
	for i := range want {
		if root[i] != want[i] {
			t.Errorf("root[%d] = %+v, want %+v", i, root[i], want[i])
		}
	}

	// One level down: the versions, and none of the shards under them.
	versions, err := b.List(ctx, "imagenet", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 {
		t.Fatalf("imagenet lists %+v, want its two versions", versions)
	}
	for _, v := range versions {
		if !v.Dir {
			t.Errorf("%q is not reported as a directory", v.Name)
		}
		if strings.Contains(v.Name, "/") {
			t.Errorf("%q is more than one level", v.Name)
		}
	}

	// And the leaves, with their sizes, which is what getattrs needs.
	shards, err := b.List(ctx, "imagenet/v17", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) != 2 || shards[0].Name != "shard-0" || shards[0].Bytes != 2 {
		t.Errorf("imagenet/v17 lists %+v, want its two shards with sizes", shards)
	}
	if shards[0].Dir {
		t.Error("a shard is reported as a directory")
	}
}

// TestAPrefixIsNotMatchedByHalfAName covers the separator: without it "v1"
// would also match "v17", and a client would be shown another version's shards.
func TestAPrefixIsNotMatchedByHalfAName(t *testing.T) {
	d, _ := newFake(t, map[string][]byte{
		"ds/v1/shard":  []byte("a"),
		"ds/v17/shard": []byte("b"),
	})
	b, err := driver.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.List(context.Background(), "ds/v1", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "shard" {
		t.Errorf("ds/v1 lists %+v, want only its own shard", got)
	}
}

// TestPagingInsideAPrefixDoesNotRepeat covers what start-after has to carry.
// A store pages on the whole key, so sending the bare name would start after
// something in another level, or after nothing at all, and the same page would
// come back for ever.
func TestPagingInsideAPrefixDoesNotRepeat(t *testing.T) {
	d, _ := newFake(t, map[string][]byte{
		"ds/v1/a": []byte("1"),
		"ds/v1/b": []byte("2"),
		"ds/v1/c": []byte("3"),
	})
	b, err := driver.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var seen []string
	after := ""
	for i := 0; i < 10; i++ {
		page, err := b.List(ctx, "ds/v1", after, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		seen = append(seen, page[0].Name)
		after = page[0].Name
	}
	if got := strings.Join(seen, ""); got != "abc" {
		t.Errorf("paging inside a prefix saw %q, want each name once", got)
	}
}
