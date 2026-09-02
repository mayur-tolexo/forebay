//go:build live

// Excluded from the default build: it needs a store, credentials and a bucket
// it is allowed to write in, so it is compiled only when asked for.
//
//	go test -c -tags live ./driver/s3driver/
//	S3_ENDPOINT=... S3_BUCKET=... S3_ACCESS_KEY=... S3_SECRET_KEY=... \
//	  S3_PREFIX=scratch/ ./s3driver.test -test.v
package s3driver

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"testing"

	"github.com/mayur-tolexo/forebay/driver"
	"github.com/mayur-tolexo/forebay/driver/conformance"
)

// liveDriver builds a driver from the environment, skipping when it is not
// configured so an unconfigured run says so rather than failing.
func liveDriver(t *testing.T) (*Driver, string) {
	t.Helper()
	endpoint := os.Getenv("S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3_ENDPOINT is not set")
	}
	d, err := New(Config{
		Endpoint:  endpoint,
		Bucket:    os.Getenv("S3_BUCKET"),
		Region:    os.Getenv("S3_REGION"),
		AccessKey: os.Getenv("S3_ACCESS_KEY"),
		SecretKey: os.Getenv("S3_SECRET_KEY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, os.Getenv("S3_PREFIX")
}

// TestLiveConformance runs the suite every driver has to pass against a real
// store, over a fixture it writes and removes.
func TestLiveConformance(t *testing.T) {
	d, prefix := liveDriver(t)
	ctx := context.Background()

	content := make([]byte, 1<<20)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	object := prefix + "fixture"
	if err := d.WriteObject(ctx, object, content); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	t.Cleanup(func() {
		if err := d.DeleteObject(ctx, object); err != nil {
			t.Errorf("the fixture was left behind: %v", err)
		}
	})

	conformance.Run(t, conformance.Fixture{
		Driver:         d,
		Object:         object,
		Content:        content,
		WritablePrefix: prefix + "scratch",
	})
}

// TestLiveRangeEdges checks the two ways a store reports a read off the end,
// which a fake can be written to agree with and a real one cannot.
func TestLiveRangeEdges(t *testing.T) {
	d, prefix := liveDriver(t)
	ctx := context.Background()

	content := []byte("0123456789")
	object := prefix + "edges"
	if err := d.WriteObject(ctx, object, content); err != nil {
		t.Fatalf("writing: %v", err)
	}
	t.Cleanup(func() {
		if err := d.DeleteObject(ctx, object); err != nil {
			t.Errorf("left behind: %v", err)
		}
	})

	size := int64(len(content))
	if got, err := d.SizeOf(ctx, object); err != nil || got != size {
		t.Errorf("SizeOf = %d, %v, want %d", got, err, size)
	}
	// A length running off the end, which S3 answers with a short 206.
	if _, err := d.ReadRange(ctx, object, 0, size+1); !errors.Is(err, driver.ErrRange) {
		t.Errorf("reading past the end = %v, want ErrRange", err)
	}
	// An offset at the end, which it answers with a 416.
	if _, err := d.ReadRange(ctx, object, size, 1); !errors.Is(err, driver.ErrRange) {
		t.Errorf("reading from the end = %v, want ErrRange", err)
	}
	if got, err := d.ReadRange(ctx, object, size-1, 1); err != nil || string(got) != "9" {
		t.Errorf("last byte = %q, %v", got, err)
	}
}
