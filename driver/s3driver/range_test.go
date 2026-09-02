package s3driver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mayur-tolexo/forebay/driver"
)

// TestIgnoredRangeIsNotTakenAsTheAnswer covers a store, or a proxy in front of
// one, answering a ranged GET with the whole object. The bytes are the right
// length from the wrong offset, so checking length alone cannot catch it.
func TestIgnoredRangeIsNotTakenAsTheAnswer(t *testing.T) {
	body := []byte("AAAABBBBCCCCDDDD")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	defer srv.Close()

	d, err := New(Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "k", SecretKey: "s", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.ReadRange(context.Background(), "o", 8, 4)
	if err == nil {
		t.Fatalf("a whole-object answer to a ranged read returned %q, want an error", got)
	}
	if errors.Is(err, driver.ErrRange) {
		t.Errorf("err = %v, want a failure rather than ErrRange: the range was fine, the store ignored it", err)
	}
}
