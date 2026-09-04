// Package s3driver is a driver over an S3-compatible object store.
//
// It signs its own requests with SigV4 over net/http, because this project
// takes no dependencies and the alternative is a vendor SDK an order of
// magnitude larger than the driver.
//
// Path style addressing only, which is what Ceph RGW and MinIO serve and what
// works without a wildcard DNS entry per bucket. Amazon's own endpoint wants
// virtual-hosted style for buckets made since 2020, so this does not reach it.
//
// A Driver holds nothing that changes, so the fast tier above it can read
// blocks in parallel.
package s3driver

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mayur-tolexo/forebay/driver"
)

// ErrBadObject rejects a key this driver will not send.
var ErrBadObject = errors.New("s3driver: object name is not a usable key")

// Config points a driver at one bucket.
type Config struct {
	// Endpoint is the scheme and host of the store, without a bucket.
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	// HTTPClient is optional. The default carries a timeout, since a driver
	// with none turns a hung backend into a hung read that never returns.
	HTTPClient *http.Client
	// Attempts bounds how often a transient failure is sent again, the first
	// try included. Zero takes the default; one turns retrying off.
	Attempts int
}

// Driver reads objects from one bucket.
type Driver struct {
	endpoint *url.URL
	bucket   string
	creds    credentials
	client   *http.Client
	now      func() time.Time
	attempts int
	backoff  time.Duration
}

// New points a driver at a bucket, refusing a configuration that cannot work
// rather than failing later on the first read.
func New(c Config) (*Driver, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("s3driver: endpoint: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("s3driver: endpoint %q needs a scheme and a host", c.Endpoint)
	}
	if c.Bucket == "" {
		return nil, errors.New("s3driver: no bucket")
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return nil, errors.New("s3driver: no credentials")
	}
	region := c.Region
	if region == "" {
		// What a store that does not care about regions still expects to see
		// in the signature, and what RGW is configured with by default.
		region = "us-east-1"
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	attempts := c.Attempts
	if attempts <= 0 {
		attempts = 3
	}
	return &Driver{
		endpoint: u,
		bucket:   c.Bucket,
		creds:    credentials{accessKey: c.AccessKey, secretKey: c.SecretKey, region: region},
		client:   client,
		now:      time.Now,
		attempts: attempts,
		// Short, because a read is retried while an NFS client waits on it.
		backoff: 100 * time.Millisecond,
	}, nil
}

// Declare says what this bucket can do.
//
// Snapshot and clone are absent: S3 CopyObject moves the bytes, and a clone
// that copies is not a clone. Declaring them would be the emulation the
// contract exists to forbid.
func (d *Driver) Declare() driver.Declaration {
	return driver.Declaration{
		Contract: 1,
		Capabilities: []driver.Capability{
			driver.ReadRange, driver.ObjectSize, driver.WriteObject, driver.DeleteObject,
			driver.ListObjects,
		},
	}
}

// listResult is as much of ListObjectsV2's answer as a level needs.
type listResult struct {
	Contents []struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

// List returns one level under a prefix, in name order.
//
// An object store has no directories, so the level is derived: a delimiter
// makes the store report everything sharing a prefix as one common prefix,
// which is what a directory is here. Asking without one would return every
// object beneath, which for a dataset is every shard rather than every version.
func (d *Driver) List(ctx context.Context, prefix, after string, limit int) ([]driver.Entry, error) {
	// A prefix is a directory, so it ends in the separator: without it
	// "v1" would also match "v17".
	full := prefix
	if full != "" && !strings.HasSuffix(full, "/") {
		full += "/"
	}

	q := url.Values{}
	q.Set("list-type", "2")
	q.Set("delimiter", "/")
	q.Set("max-keys", strconv.Itoa(limit))
	if full != "" {
		q.Set("prefix", full)
	}
	if after != "" {
		q.Set("start-after", full+after)
	}

	u := *d.endpoint
	u.Path = "/" + d.bucket
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("s3driver: %w", err)
	}
	resp, err := d.do(req, emptyPayload)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if err := errorFor(resp); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("s3driver: reading the listing: %w", err)
	}
	var out listResult
	if err := xml.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("s3driver: reading the listing: %w", err)
	}

	entries := make([]driver.Entry, 0, len(out.Contents)+len(out.CommonPrefixes))
	for _, p := range out.CommonPrefixes {
		name := strings.TrimSuffix(strings.TrimPrefix(p.Prefix, full), "/")
		if name != "" {
			entries = append(entries, driver.Entry{Name: name, Dir: true})
		}
	}
	for _, c := range out.Contents {
		name := strings.TrimPrefix(c.Key, full)
		// The prefix itself comes back as a key when something created a
		// zero-length marker for it. It is not a name under the level.
		//
		// Nothing checks for a separator in the name: with a delimiter
		// set a store returns those as common prefixes, and one that did
		// not would be failing the conformance suite's own rule that a
		// listing returns one level. Guarding here would hide that.
		if name == "" {
			continue
		}
		entries = append(entries, driver.Entry{Name: name, Bytes: c.Size})
	}
	// Sorted together, because the store returns prefixes and keys in two
	// lists and a caller paging on the last name it saw needs one order.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

// ReadRange reads length bytes from offset.
//
// A range reaching past the end is an error rather than a short read. S3 says
// so two different ways: an offset at or past the end is a 416, but a length
// running off the end is a 206 holding fewer bytes than were asked for, which
// is why the answer is measured rather than trusted.
func (d *Driver) ReadRange(ctx context.Context, object string, offset, length int64) ([]byte, error) {
	if length == 0 {
		// Still has to establish the object is there and the offset is inside
		// it, or a read of nothing from a name that does not exist succeeds.
		size, err := d.SizeOf(ctx, object)
		if err != nil {
			return nil, err
		}
		if offset > size {
			return nil, fmt.Errorf("%w: offset %d, object is %d", driver.ErrRange, offset, size)
		}
		return []byte{}, nil
	}

	resp, err := d.send(ctx, http.MethodGet, object, nil, func(r *http.Request) {
		r.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return nil, fmt.Errorf("%w: %d bytes from %d in %s", driver.ErrRange, length, offset, object)
	}
	if err := errorFor(resp); err != nil {
		return nil, err
	}
	// A 200 to a ranged GET is the whole object: its first bytes are the right
	// length from the wrong offset, which no later check can notice.
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("s3driver: %s answered %s to a ranged read, not 206",
			object, resp.Status)
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, length))
	if err != nil {
		return nil, fmt.Errorf("s3driver: reading %s: %w", object, err)
	}
	if int64(len(buf)) != length {
		return nil, fmt.Errorf("%w: asked %d bytes from %d in %s, got %d",
			driver.ErrRange, length, offset, object, len(buf))
	}
	return buf, nil
}

// SizeOf reports how many bytes an object holds, from the HEAD its own
// Content-Length answers.
func (d *Driver) SizeOf(ctx context.Context, object string) (int64, error) {
	resp, err := d.send(ctx, http.MethodHead, object, nil, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if err := errorFor(resp); err != nil {
		return 0, err
	}
	// ContentLength is -1 when the header is missing, which is not a size.
	if resp.ContentLength < 0 {
		return 0, fmt.Errorf("s3driver: %s answered no content length", object)
	}
	return resp.ContentLength, nil
}

// WriteObject creates an object. S3 replaces one of the same name rather than
// refusing, and this does not pretend otherwise.
func (d *Driver) WriteObject(ctx context.Context, object string, data []byte) error {
	resp, err := d.send(ctx, http.MethodPut, object, data, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return errorFor(resp)
}

// DeleteObject removes one.
func (d *Driver) DeleteObject(ctx context.Context, object string) error {
	resp, err := d.send(ctx, http.MethodDelete, object, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return errorFor(resp)
}

// SnapshotObject is not something this driver does.
func (d *Driver) SnapshotObject(ctx context.Context, object string) (string, error) {
	return "", fmt.Errorf("%w: %s", driver.ErrNotSupported, driver.Snapshot)
}

// CloneObject is not something this driver does. CopyObject would satisfy the
// signature by copying every byte, which is what the caller asked to avoid.
func (d *Driver) CloneObject(ctx context.Context, from, to string) error {
	return fmt.Errorf("%w: %s", driver.ErrNotSupported, driver.Clone)
}

// request builds an unsigned request for one object.
func (d *Driver) request(ctx context.Context, method, object string, body []byte) (*http.Request, error) {
	if err := checkObject(object); err != nil {
		return nil, err
	}
	u := *d.endpoint
	u.Path = "/" + d.bucket + "/" + object

	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return nil, fmt.Errorf("s3driver: %w", err)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	return req, nil
}

// do signs a request and sends it once.
func (d *Driver) do(req *http.Request, payloadHash string) (*http.Response, error) {
	sign(req, d.creds, payloadHash, d.now())
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3driver: %s %s: %w", req.Method, req.URL.Path, err)
	}
	return resp, nil
}

// send retries a request the store failed to answer.
//
// Nothing above this retries: the fast tier turns a backend error into a read
// error, and the NFS client it reaches cannot be told to try again. A reset
// connection or a 503 would become an IO error on a file that is fine.
//
// Every request here is a read, a whole-object write or a delete of one key,
// so repeating one cannot land somewhere a single attempt would not. Rebuilt
// each attempt rather than resent, so retrying does not rest on the transport
// rewinding a body it has already written.
func (d *Driver) send(ctx context.Context, method, object string, body []byte, prepare func(*http.Request)) (*http.Response, error) {
	payload := emptyPayload
	if body != nil {
		payload = sha256Hex(body)
	}
	var last error
	for attempt := 0; ; attempt++ {
		req, err := d.request(ctx, method, object, body)
		if err != nil {
			return nil, err
		}
		if prepare != nil {
			prepare(req)
		}
		resp, err := d.do(req, payload)
		switch {
		case err != nil:
			last = err
		case !transient(resp.StatusCode):
			return resp, nil
		default:
			last = fmt.Errorf("s3driver: %s %s: %s", method, object, resp.Status)
			drain(resp)
		}
		if attempt+1 >= d.attempts {
			return nil, fmt.Errorf("%w (after %d attempts)", last, d.attempts)
		}
		if err := pause(ctx, d.backoff<<attempt); err != nil {
			return nil, err
		}
	}
}

// transient reports a status worth sending again. Everything else is the
// store's answer, including a refusal, and repeating it changes nothing.
func transient(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// drain empties a response far enough for the connection to be reused, since a
// retry that opens a new one costs more than the read it is retrying.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
	resp.Body.Close()
}

// pause waits between attempts, or stops early when the caller has given up.
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// checkObject rejects a key that cannot address an object.
func checkObject(object string) error {
	if object == "" || strings.HasPrefix(object, "/") {
		return fmt.Errorf("%w: %q", ErrBadObject, object)
	}
	return nil
}

// denied reports a refusal the credential caused rather than the store being
// unhappy.
//
// Matched on the status as well as the code, since stores disagree about which
// code they send: what they agree on is 403 for a credential that may not, and
// a store sending 403 for something else is telling the same lie to everyone.
func denied(status int, code string) bool {
	return status == http.StatusForbidden || code == "AccessDenied"
}

// s3Error is the body S3 returns with a failure.
type s3Error struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// errorFor turns a failing response into an error carrying the store's own
// reason, which is the difference between a fixable message and "500".
func errorFor(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// Bounded: an error body is small, and a proxy answering with a web page
	// should not be read into memory in full.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var e s3Error
	var out error
	if err := xml.Unmarshal(body, &e); err == nil && e.Code != "" {
		out = fmt.Errorf("s3driver: %s: %s (%s)", resp.Status, e.Code, e.Message)
	} else {
		out = fmt.Errorf("s3driver: %s", resp.Status)
	}
	// Wrapped rather than formatted in, or errors.Is would never match and the
	// distinction would exist only in the text a person reads.
	if denied(resp.StatusCode, e.Code) {
		return fmt.Errorf("%w: %w", driver.ErrDenied, out)
	}
	return out
}

// sha256Hex is the payload hash a signed request carries.
func sha256Hex(b []byte) string {
	return hex.EncodeToString(hash(b))
}
