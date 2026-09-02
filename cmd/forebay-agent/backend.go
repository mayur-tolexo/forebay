package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/mayur-tolexo/forebay/driver"
	"github.com/mayur-tolexo/forebay/driver/filedriver"
	"github.com/mayur-tolexo/forebay/driver/s3driver"
)

// Credentials come from the environment rather than a flag: a flag is visible
// in ps, in a container's own spec and in whatever recorded the command.
const (
	accessKeyEnv = "FOREBAY_S3_ACCESS_KEY"
	secretKeyEnv = "FOREBAY_S3_SECRET_KEY"
)

// errNoBackend reports that nothing was chosen to read from.
var errNoBackend = errors.New("serving needs a backend, since a miss is answered from the durable one")

// backendOptions is what the flags say to read from.
type backendOptions struct {
	Dir      string
	Endpoint string
	Bucket   string
	Region   string
}

// openBackend picks the durable backend from what was configured.
//
// One or the other, never both: a node reading from two stores would serve
// whichever the flags happened to name first, and the difference is invisible
// in what a client gets back.
func openBackend(o backendOptions) (*driver.Backend, error) {
	dir, s3 := o.Dir != "", o.Endpoint != "" || o.Bucket != ""
	switch {
	case dir && s3:
		return nil, errors.New("--backend-dir and --backend-s3-endpoint name two backends, pass one")
	case !dir && !s3:
		return nil, errNoBackend
	case dir:
		d, err := filedriver.New(o.Dir)
		if err != nil {
			return nil, fmt.Errorf("opening the backend: %w", err)
		}
		return driver.Open(d)
	}

	if o.Endpoint == "" || o.Bucket == "" {
		return nil, errors.New("an S3 backend needs both --backend-s3-endpoint and --backend-s3-bucket")
	}
	// An endpoint carrying its own credentials would put them in the startup
	// line and in the tier's keys, which is what reading them from the
	// environment exists to avoid.
	if u, err := url.Parse(o.Endpoint); err == nil && u.User != nil {
		return nil, fmt.Errorf("the endpoint carries credentials, pass them in %s and %s", accessKeyEnv, secretKeyEnv)
	}
	access, secret := os.Getenv(accessKeyEnv), os.Getenv(secretKeyEnv)
	if access == "" || secret == "" {
		return nil, fmt.Errorf("an S3 backend reads its credentials from %s and %s", accessKeyEnv, secretKeyEnv)
	}
	d, err := s3driver.New(s3driver.Config{
		Endpoint:  o.Endpoint,
		Bucket:    o.Bucket,
		Region:    o.Region,
		AccessKey: access,
		SecretKey: secret,
	})
	if err != nil {
		return nil, fmt.Errorf("opening the backend: %w", err)
	}
	return driver.Open(d)
}

// scope names the backend in the fast tier's keys. Two backends must not share
// one, or a block cached for an object answers for another backend's.
func (o backendOptions) scope() (string, error) {
	if o.Dir != "" {
		return filepath.Abs(o.Dir)
	}
	return "s3://" + endpointHost(o.Endpoint) + "/" + o.Bucket, nil
}

// endpointHost reduces an endpoint to its host, which is what names a store
// without the scheme or anything a URL can carry in front of it.
func endpointHost(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return endpoint
}

// describe names the backend for the line the agent prints at startup, with
// no credential in it.
func describe(o backendOptions) string {
	if o.Dir != "" {
		return "directory " + o.Dir
	}
	return fmt.Sprintf("s3 %s bucket %s", endpointHost(o.Endpoint), o.Bucket)
}
