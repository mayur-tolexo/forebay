package volumes

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Report is one volume request, as it travels from the node plugin.
type Report struct {
	// ID is the volume the orchestrator named, which is what a later
	// unpublish will name too.
	ID string `json:"id"`
	// Bytes is how large the dataset is, from the controller that resolved
	// it. Zero means it was not known rather than that it is empty.
	Bytes int64 `json:"bytes"`
}

// Handler serves the reporting surface for one registry.
//
// Guarded by the same token as the rest of the agent's surface. What arrives
// here raises the shortfall the node reclaims against, so anything that can
// post to it can make a node throw its cache away.
func Handler(r *Registry, token string) http.Handler {
	s := &server{reg: r, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /volumes", s.guard(s.published))
	mux.HandleFunc("DELETE /volumes/{id...}", s.guard(s.unpublished))
	mux.HandleFunc("GET /volumes", s.guard(s.list))
	return mux
}

type server struct {
	reg   *Registry
	token string
}

// guard refuses anything without the token, compared in constant time so the
// answers do not leak its prefix.
func (s *server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		given := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(given), []byte(s.token)) != 1 {
			http.Error(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// published records a volume now served here.
func (s *server) published(w http.ResponseWriter, r *http.Request) {
	var rep Report
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&rep); err != nil {
		http.Error(w, "unreadable report: "+err.Error(), http.StatusBadRequest)
		return
	}
	if rep.ID == "" {
		http.Error(w, "a report names the volume it is about", http.StatusBadRequest)
		return
	}
	s.reg.Published(rep.ID, rep.Bytes)
	w.WriteHeader(http.StatusNoContent)
}

// unpublished forgets one.
//
// A volume that was never recorded is not an error: the node plugin repeats an
// unpublish it did not hear the answer to, and the agent may have restarted
// since the publish, in which case it has no memory of it and the caller is
// asking for a state that already holds.
func (s *server) unpublished(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "an unpublish names the volume it is about", http.StatusBadRequest)
		return
	}
	s.reg.Unpublished(id)
	w.WriteHeader(http.StatusNoContent)
}

// list says what the node has been asked to serve, for an operator looking at
// why a reclaim happened.
func (s *server) list(w http.ResponseWriter, r *http.Request) {
	count, size := s.reg.Requested()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Volumes int   `json:"volumes"`
		Bytes   int64 `json:"bytes"`
	}{count, size})
}

// Client reports volume requests to an agent.
//
// It is the node plugin's half. Every method is best effort by design: the
// driver reports and never asks, so a failure here is logged by the caller and
// the mount goes ahead regardless.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient points one at an agent.
func NewClient(base, token string, timeout time.Duration) *Client {
	return &Client{
		base:  strings.TrimSuffix(base, "/"),
		token: token,
		http:  &http.Client{Timeout: timeout},
	}
}

// Published tells the agent a dataset landed.
func (c *Client) Published(ctx context.Context, id string, size int64) error {
	body, err := json.Marshal(Report{ID: id, Bytes: size})
	if err != nil {
		return err
	}
	return c.send(ctx, http.MethodPost, c.base+"/volumes", body)
}

// Unpublished tells it the dataset is gone.
//
// The id is escaped a segment at a time. It comes from a volume's handle,
// which is whatever was written into the object, so the slashes that name it
// have to survive while anything else must not become part of the URL.
func (c *Client) Unpublished(ctx context.Context, id string) error {
	segments := strings.Split(id, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return c.send(ctx, http.MethodDelete, c.base+"/volumes/"+strings.Join(segments, "/"), nil)
}

// send makes one request and reads the outcome.
func (c *Client) send(ctx context.Context, method, url string, body []byte) error {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		what, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("the agent answered %s: %s", resp.Status, bytes.TrimSpace(what))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}
