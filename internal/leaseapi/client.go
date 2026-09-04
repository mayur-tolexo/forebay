package leaseapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client proposes leases to one node.
type Client struct {
	http  *http.Client
	base  string
	token string
}

// NewClient returns a client for the agent at an address.
func NewClient(address, token string, timeout time.Duration) *Client {
	return &Client{
		http:  &http.Client{Timeout: timeout},
		base:  "http://" + address,
		token: token,
	}
}

// Propose asks the node to lend capacity, and returns what it decided.
//
// A node that could not be reached is a Decision rather than an error, for the
// same reason a refusal is: a planner has to do something with either, and
// making one an error and the other a value would put the two answers on
// different paths through every caller.
func (c *Client) Propose(ctx context.Context, p Proposal) (Decision, error) {
	body, err := encode(p)
	if err != nil {
		return Decision{}, err
	}
	resp, err := c.send(ctx, http.MethodPost, "/leases", body)
	if err != nil {
		return Decision{Reason: err.Error(), Refusal: Unavailable}, nil
	}
	defer resp.Body.Close()

	answer, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return Decision{Reason: err.Error(), Refusal: Unavailable}, nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// Named rather than folded into unavailable, because a wrong token is
		// a configuration mistake that no retry and no other node will fix.
		return Decision{Reason: "the node refused this control plane's token", Refusal: Malformed}, nil
	}
	var d Decision
	if err := decode(answer, &d); err != nil {
		return Decision{Reason: fmt.Sprintf("unreadable answer: %v", err), Refusal: Unavailable}, nil
	}
	return d, nil
}

// Release gives a lease back. Releasing one the node does not hold is not an
// error, so a caller retrying after a timeout can stop.
func (c *Client) Release(ctx context.Context, id string) error {
	resp, err := c.send(ctx, http.MethodDelete, "/leases/"+id, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("leaseapi: releasing %s: %s", id, resp.Status)
	}
	return nil
}

// Capacity reads what the node says about its own pool.
func (c *Client) Capacity(ctx context.Context) (Capacity, error) {
	resp, err := c.send(ctx, http.MethodGet, "/capacity", nil)
	if err != nil {
		return Capacity{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return Capacity{}, err
	}
	if resp.StatusCode >= 300 {
		return Capacity{}, fmt.Errorf("leaseapi: reading capacity: %s", resp.Status)
	}
	var out Capacity
	return out, decode(body, &out)
}

// send makes one request with the token on it.
func (c *Client) send(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}
