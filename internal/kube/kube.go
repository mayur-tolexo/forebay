// Package kube reads and writes custom resources on the API server.
//
// Hand-written against the REST API rather than built on a generated client,
// because the types here are the small subset of one resource that this
// project declares, and pulling in a client library would be the largest
// dependency in the repository by an order of magnitude.
//
// The agent does not use this. RFC-0004 requires that reclamation never needs
// the control plane, so a node reads pods from its own kubelet and this
// package belongs to the controller, which is allowed to stop when the API
// server does.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Where a pod finds its own credentials and the cluster's certificate.
//
// Variables rather than constants so a test can point them somewhere it can
// write. Nothing outside a test changes them.
var (
	tokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	caPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// Config points a client at an API server.
type Config struct {
	// Host is the API server's base URL.
	Host string
	// Token authorises every request. Bearer rather than client certificates,
	// since a pod is given one and not the other.
	Token string
	// CA verifies the server. Empty means the system pool, which is what a
	// kubeconfig pointing at a public endpoint wants.
	CA []byte
	// Timeout bounds one request. A watch is exempt, since it is meant to
	// stay open.
	Timeout time.Duration
}

// InCluster builds a config from what the pod was given.
func InCluster() (Config, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return Config{}, fmt.Errorf("kube: not running in a cluster, since KUBERNETES_SERVICE_HOST and _PORT are not both set")
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return Config{}, fmt.Errorf("kube: reading the service account token: %w", err)
	}
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return Config{}, fmt.Errorf("kube: reading the cluster certificate: %w", err)
	}
	return Config{
		Host:    "https://" + net.JoinHostPort(host, port),
		Token:   strings.TrimSpace(string(token)),
		CA:      ca,
		Timeout: 30 * time.Second,
	}, nil
}

// Client talks to one API server.
type Client struct {
	base    string
	token   string
	http    *http.Client
	timeout time.Duration
}

// New builds a client, refusing a configuration it could not use.
func New(cfg Config) (*Client, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("kube: no API server")
	}
	if _, err := url.Parse(cfg.Host); err != nil {
		return nil, fmt.Errorf("kube: API server %q: %w", cfg.Host, err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if len(cfg.CA) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(cfg.CA) {
			return nil, fmt.Errorf("kube: the cluster certificate did not parse")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		base:  strings.TrimSuffix(cfg.Host, "/"),
		token: cfg.Token,
		// No timeout on the client itself, since a watch shares it and is
		// meant to stay open. One request bounds itself with a context.
		http:    &http.Client{Transport: transport},
		timeout: timeout,
	}, nil
}

// Resource names one custom resource kind.
type Resource struct {
	Group, Version, Plural string
	// Namespace empty means across all of them.
	Namespace string
}

// path builds the collection URL for a resource, and one item when named.
func (c *Client) path(r Resource, name string) string {
	var b strings.Builder
	b.WriteString(c.base)
	b.WriteString("/apis/")
	b.WriteString(url.PathEscape(r.Group))
	b.WriteByte('/')
	b.WriteString(url.PathEscape(r.Version))
	if r.Namespace != "" {
		b.WriteString("/namespaces/")
		b.WriteString(url.PathEscape(r.Namespace))
	}
	b.WriteByte('/')
	b.WriteString(url.PathEscape(r.Plural))
	if name != "" {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(name))
	}
	return b.String()
}

// do sends one request, bounded by the client's timeout.
func (c *Client) do(ctx context.Context, method, u string, body []byte, contentType string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return nil, fmt.Errorf("kube: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kube: %s %s: %w", method, u, err)
	}
	defer resp.Body.Close()

	// Bounded: an API server error is small, and a proxy answering with a page
	// should not be read into memory in full.
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("kube: reading the answer: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, statusError(resp.StatusCode, out)
	}
	return out, nil
}

// List reads every object of a kind, decoding items into out.
func (c *Client) List(ctx context.Context, r Resource, out any) error {
	body, err := c.do(ctx, http.MethodGet, c.path(r, ""), nil, "")
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// PatchStatus merges a status into one object.
//
// A merge patch rather than a replace, so a controller writing what it
// observed cannot silently drop a field written by a newer version of itself
// or by something else.
func (c *Client) PatchStatus(ctx context.Context, r Resource, name string, status any) error {
	body, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		return fmt.Errorf("kube: encoding the status: %w", err)
	}
	_, err = c.do(ctx, http.MethodPatch, c.path(r, name)+"/status", body, "application/merge-patch+json")
	return err
}

// Status is what the API server said went wrong, which is worth keeping: a
// controller that reports "403" and not which resource it was refused on
// leaves an operator to guess at the RBAC it is missing.
type Status struct {
	Code    int
	Reason  string
	Message string
}

func (s *Status) Error() string {
	if s.Message == "" {
		return fmt.Sprintf("kube: %d %s", s.Code, s.Reason)
	}
	return fmt.Sprintf("kube: %d %s: %s", s.Code, s.Reason, s.Message)
}

// NotFound reports whether an error is the API server saying so, which a
// controller treats as an object that has gone rather than as a failure.
func NotFound(err error) bool {
	var s *Status
	return errors.As(err, &s) && s.Code == http.StatusNotFound
}

// statusError turns a failing response into one carrying the server's reason.
func statusError(code int, body []byte) error {
	s := &Status{Code: code}
	var parsed struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		s.Reason, s.Message = parsed.Reason, parsed.Message
	}
	if s.Reason == "" {
		s.Reason = http.StatusText(code)
	}
	return s
}
