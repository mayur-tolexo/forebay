package checkpoint_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayur-tolexo/forebay/internal/checkpoint"
	"github.com/mayur-tolexo/forebay/internal/lease"
	"github.com/mayur-tolexo/forebay/internal/pool"
)

// fakeNode is a node whose capacity is a number and whose extents are files.
type fakeNode struct {
	mu          sync.Mutex
	dir         string
	guaranteed  pool.Bytes
	granted     map[string]lease.Lease
	released    []string
	refuseGrant error
}

func newNode(t *testing.T, guaranteed pool.Bytes) *fakeNode {
	t.Helper()
	return &fakeNode{dir: t.TempDir(), guaranteed: guaranteed, granted: map[string]lease.Lease{}}
}

func (n *fakeNode) GuaranteedFree() pool.Bytes {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.guaranteed
}

func (n *fakeNode) Grant(l lease.Lease, now time.Time) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.refuseGrant != nil {
		return n.refuseGrant
	}
	if l.Size > n.guaranteed {
		return fmt.Errorf("the node cannot promise %s", l.Size)
	}
	// The extent, sized like the agent's: a file of the lease's size that the
	// staging write opens rather than creates.
	f, err := os.OpenFile(filepath.Join(n.dir, l.ID), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(int64(l.Size)); err != nil {
		return err
	}
	n.granted[l.ID] = l
	n.guaranteed -= l.Size
	return nil
}

func (n *fakeNode) Release(id string, now time.Time) (pool.Bytes, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	l, held := n.granted[id]
	if !held {
		return 0, fmt.Errorf("no lease %s", id)
	}
	delete(n.granted, id)
	n.guaranteed += l.Size
	n.released = append(n.released, id)
	os.Remove(filepath.Join(n.dir, id))
	return l.Size, nil
}

func (n *fakeNode) ExtentPath(id string) (string, error) { return filepath.Join(n.dir, id), nil }

func (n *fakeNode) holding() []lease.Lease {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]lease.Lease, 0, len(n.granted))
	for _, l := range n.granted {
		out = append(out, l)
	}
	return out
}

// fakeStore is a backend that records what it was given, and can refuse or
// block.
type fakeStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	err     error
	gate    chan struct{}
}

func newStore() *fakeStore { return &fakeStore{objects: map[string][]byte{}} }

func (s *fakeStore) WriteObjectFrom(ctx context.Context, object string, src io.ReadSeeker, size int64) error {
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// Checked after the gate as well as in it. A cancelled context and an
	// open gate are both ready at once, so the select alone would pick
	// between them and the test that turns on cancellation would pass half
	// the time.
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	body, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	if int64(len(body)) != size {
		return fmt.Errorf("declared %d bytes and %d arrived", size, len(body))
	}
	s.objects[object] = body
	return nil
}

func (s *fakeStore) get(object string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[object]
	return b, ok
}

func stager(t *testing.T, n checkpoint.Node, s checkpoint.Store) *checkpoint.Stager {
	t.Helper()
	st, err := checkpoint.New(checkpoint.Config{
		Node: n, Store: s, Term: time.Hour,
		Now: func() time.Time { return time.Unix(0, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func request(id string, size int) checkpoint.Request {
	return checkpoint.Request{
		ID: id, Tenant: "team", Object: "runs/17/" + id,
		Bytes: pool.Bytes(size),
	}
}

func TestADurableCheckpointIsInTheBackendAndTheCapacityIsGivenBack(t *testing.T) {
	// The default acknowledgement. It cannot lose the writer's work, and the
	// lease is released only once the bytes are somewhere that survives the
	// node.
	n, s := newNode(t, 1<<20), newStore()
	body := []byte("the state of a model, mid-run")
	c, err := stager(t, n, s).Stage(t.Context(), request("rank-0", len(body)), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if c.Ack() != checkpoint.Durable {
		t.Errorf("acknowledged %q, want durable", c.Ack())
	}
	got, ok := s.get("runs/17/rank-0")
	if !ok {
		t.Fatal("the backend does not have it")
	}
	if !bytes.Equal(got, body) {
		t.Errorf("the backend holds %q", got)
	}
	if held := n.holding(); len(held) != 0 {
		t.Errorf("the node is still holding %v", held)
	}
}

func TestAStagedCheckpointIsAcknowledgedBeforeItIsDurable(t *testing.T) {
	// The whole reason the fast acknowledgement exists: a rank stops waiting
	// once the bytes are on local disk, and the upload carries on without it.
	n, s := newNode(t, 1<<20), newStore()
	s.gate = make(chan struct{})
	body := []byte("state a job will accept losing to a node failure")

	r := request("rank-1", len(body))
	r.Ack = checkpoint.Staged
	c, err := stager(t, n, s).Stage(t.Context(), r, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if c.Ack() != checkpoint.Staged {
		t.Fatalf("acknowledged %q, want staged", c.Ack())
	}
	// Not durable yet, and the capacity is still held, because it holds the
	// only copy.
	if _, ok := s.get("runs/17/rank-1"); ok {
		t.Error("the backend has it already, so nothing was staged")
	}
	if len(n.holding()) != 1 {
		t.Error("the staging capacity was released before the bytes were durable")
	}

	close(s.gate)
	if err := c.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.get("runs/17/rank-1"); !ok || !bytes.Equal(got, body) {
		t.Errorf("the backend holds %q %v", got, ok)
	}
	if held := n.holding(); len(held) != 0 {
		t.Errorf("the node is still holding %v after durability", held)
	}
}

func TestStagingUsesCapacityNobodyCanTakeBack(t *testing.T) {
	// The one exception to the rule that borrowed data is regenerable. A
	// reclaim taking the only copy of a job's progress would break the
	// constraint the whole project rests on.
	n, s := newNode(t, 1<<20), newStore()
	s.gate = make(chan struct{})
	r := request("rank-2", 8)
	r.Ack = checkpoint.Staged
	if _, err := stager(t, n, s).Stage(t.Context(), r, bytes.NewReader([]byte("8 bytes."))); err != nil {
		t.Fatal(err)
	}
	held := n.holding()
	if len(held) != 1 {
		t.Fatalf("holding %d leases", len(held))
	}
	if held[0].Class != lease.Guaranteed {
		t.Errorf("staged into %s capacity, which can be taken back mid-checkpoint", held[0].Class)
	}
	close(s.gate)
}

func TestACheckpointTooLargeForTheNodeIsRefusedBeforeItWrites(t *testing.T) {
	// A rank that learns this after it has stopped computing has learned it at
	// the worst moment. It writes straight through instead, which is what it
	// would have done without this feature.
	n, s := newNode(t, 1<<10), newStore()
	_, err := stager(t, n, s).Stage(t.Context(), request("rank-3", 1<<20), bytes.NewReader(nil))
	if !errors.Is(err, checkpoint.ErrTooLarge) {
		t.Fatalf("got %v, want it to be too large", err)
	}
	if len(n.holding()) != 0 {
		t.Error("capacity was held for a checkpoint that was refused")
	}
}

func TestACheckpointLargerThanItsReservationIsTheFrameworksError(t *testing.T) {
	// Reported rather than truncated to fit. A checkpoint silently cut down is
	// a restart that reads half a model.
	n, s := newNode(t, 1<<20), newStore()
	body := bytes.Repeat([]byte("x"), 64)
	_, err := stager(t, n, s).Stage(t.Context(), request("rank-4", 16), bytes.NewReader(body))
	if !errors.Is(err, checkpoint.ErrOverran) {
		t.Fatalf("got %v, want it to have overrun", err)
	}
	if _, ok := s.get("runs/17/rank-4"); ok {
		t.Error("a checkpoint that overran was made durable")
	}
	// Released, because it is not a fallback for anything and holding
	// capacity for it would hold it forever.
	if len(n.holding()) != 0 {
		t.Error("capacity is still held for a checkpoint that overran")
	}
}

func TestAnUploadThatNeverCompletesKeepsTheCapacity(t *testing.T) {
	// Better than releasing capacity that still holds the only copy. The
	// operator sees a lease that is not shrinking, which is the design's own
	// answer.
	n, s := newNode(t, 1<<20), newStore()
	s.err = errors.New("the backend is unreachable")
	body := []byte("state with nowhere durable to go")
	_, err := stager(t, n, s).Stage(t.Context(), request("rank-5", len(body)), bytes.NewReader(body))
	if err == nil {
		t.Fatal("an upload that failed was reported as durable")
	}
	held := n.holding()
	if len(held) != 1 {
		t.Fatalf("holding %d leases, want the staged one kept", len(held))
	}
	if held[0].ID != "rank-5" {
		t.Errorf("holding %s", held[0].ID)
	}
}

func TestAStagedUploadThatFailsIsReportedToWhoeverWaits(t *testing.T) {
	// A writer that took the fast acknowledgement and wants to know eventually
	// can find out, without having waited for it.
	n, s := newNode(t, 1<<20), newStore()
	s.err = errors.New("the backend is unreachable")
	r := request("rank-6", 8)
	r.Ack = checkpoint.Staged
	c, err := stager(t, n, s).Stage(t.Context(), r, bytes.NewReader([]byte("8 bytes.")))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(t.Context()); err == nil {
		t.Error("a failed upload reported success to the waiter")
	}
	if len(n.holding()) != 1 {
		t.Error("the capacity was released though the bytes are not durable")
	}
}

func TestTheStagedBytesSurviveOnTheNodesOwnDisk(t *testing.T) {
	// A staged acknowledgement says the bytes are on this node in capacity
	// nobody may take. That has to be true of the extent, not of a buffer.
	n, s := newNode(t, 1<<20), newStore()
	s.gate = make(chan struct{})
	body := []byte("bytes that outlive the writer")
	r := request("rank-7", len(body))
	r.Ack = checkpoint.Staged
	if _, err := stager(t, n, s).Stage(t.Context(), r, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	path, err := n.ExtentPath("rank-7")
	if err != nil {
		t.Fatal(err)
	}
	on, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(on, body) {
		t.Errorf("the extent holds %q", on[:min(len(on), 64)])
	}
	close(s.gate)
}

func TestAWriterThatStopsWaitingDoesNotCancelTheUpload(t *testing.T) {
	// The point of the fast acknowledgement is that the writer goes away. An
	// upload on the writer's context would be cancelled by the very thing it
	// was decoupled from, and the checkpoint would never become durable.
	n, s := newNode(t, 1<<20), newStore()
	s.gate = make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	body := []byte("state whose writer has moved on")
	r := request("rank-8", len(body))
	r.Ack = checkpoint.Staged
	c, err := stager(t, n, s).Stage(ctx, r, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	close(s.gate)
	if err := c.Wait(context.Background()); err != nil {
		t.Fatalf("the upload died with its writer: %v", err)
	}
	if got, ok := s.get("runs/17/rank-8"); !ok || !bytes.Equal(got, body) {
		t.Errorf("the backend holds %q %v", got, ok)
	}
}

func TestAnUnknownAcknowledgementIsRefused(t *testing.T) {
	// There is no third word. One that meant whichever of the two the reader
	// hoped for is the confusion this design exists to prevent.
	n, s := newNode(t, 1<<20), newStore()
	r := request("rank-9", 8)
	r.Ack = "committed"
	_, err := stager(t, n, s).Stage(t.Context(), r, bytes.NewReader([]byte("8 bytes.")))
	if err == nil {
		t.Fatal("a third acknowledgement was accepted")
	}
	if !strings.Contains(err.Error(), "durable") || !strings.Contains(err.Error(), "staged") {
		t.Errorf("got %q, want it to name the two that exist", err)
	}
}

func TestAStagerNeedsANodeAndABackend(t *testing.T) {
	for _, c := range []checkpoint.Config{
		{Store: newStore(), Term: time.Hour},
		{Node: newNode(t, 1), Term: time.Hour},
		{Node: newNode(t, 1), Store: newStore()},
	} {
		if _, err := checkpoint.New(c); err == nil {
			t.Errorf("built a stager with node=%v store=%v term=%s", c.Node != nil, c.Store != nil, c.Term)
		}
	}
}

func TestARefusedReservationHoldsNothing(t *testing.T) {
	// The node's own refusal, rather than this package's arithmetic. Either
	// way nothing is held and the writer is told before it has stopped.
	n, s := newNode(t, 1<<20), newStore()
	n.refuseGrant = errors.New("the node is draining")
	_, err := stager(t, n, s).Stage(t.Context(), request("rank-10", 8), bytes.NewReader([]byte("8 bytes.")))
	if err == nil {
		t.Fatal("staging went ahead against a node that refused")
	}
	if !strings.Contains(err.Error(), "draining") {
		t.Errorf("the node's reason was lost: %v", err)
	}
	if len(n.holding()) != 0 {
		t.Error("capacity is held after a refused grant")
	}
}

func TestStagedBytesAreFlushedBeforeTheAcknowledgement(t *testing.T) {
	// A staged acknowledgement says the bytes survive a restart of the agent.
	// Bytes in the page cache do not, so the flush is what makes the word
	// mean what the document says it means.
	var flushed int
	restore := checkpoint.SyncFile(func(f *os.File) error {
		flushed++
		return f.Sync()
	})
	defer restore()

	n, s := newNode(t, 1<<20), newStore()
	body := []byte("state that has to outlive the agent")
	if _, err := stager(t, n, s).Stage(t.Context(), request("rank-11", len(body)), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if flushed != 1 {
		t.Errorf("the staged extent was flushed %d times, want once", flushed)
	}
}

// serveStaging starts the staging surface over a stager.
func serveStaging(t *testing.T, n checkpoint.Node, s checkpoint.Store) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(checkpoint.Handler(stager(t, n, s), "secret"))
	t.Cleanup(srv.Close)
	return srv
}

// post sends a checkpoint the way a writer would.
func post(t *testing.T, srv *httptest.Server, query string, body []byte, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/checkpoints?"+query, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestACheckpointArrivesOverTheAgentsOwnSurface(t *testing.T) {
	n, s := newNode(t, 1<<20), newStore()
	srv := serveStaging(t, n, s)
	body := []byte("a checkpoint sent by a rank")

	resp := post(t, srv, "id=rank-0&tenant=team&object=runs/17/rank-0", body, "secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		what, _ := io.ReadAll(resp.Body)
		t.Fatalf("got %s: %s", resp.Status, what)
	}
	var out checkpoint.Outcome
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	// The default, and it says what it means: a writer that has not chosen
	// gets the acknowledgement that cannot lose its work.
	if out.Ack != string(checkpoint.Durable) {
		t.Errorf("acknowledged %q", out.Ack)
	}
	if out.Survives == "" || out.Lost == "" {
		t.Errorf("the acknowledgement did not say what it costs: %+v", out)
	}
	if got, ok := s.get("runs/17/rank-0"); !ok || !bytes.Equal(got, body) {
		t.Errorf("the backend holds %q %v", got, ok)
	}
}

func TestANodeThatCannotPromiseTheCapacityIsAConflict(t *testing.T) {
	// The writer's answer is to write straight through, which is what it
	// would have done without this feature. That is a different response from
	// a request no node would take.
	n, s := newNode(t, 8), newStore()
	srv := serveStaging(t, n, s)
	resp := post(t, srv, "id=rank-1&object=runs/17/rank-1", bytes.Repeat([]byte("x"), 4096), "secret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("got %s, want a conflict", resp.Status)
	}
}

func TestARequestNoNodeWouldTakeIsTheWritersError(t *testing.T) {
	n, s := newNode(t, 1<<20), newStore()
	srv := serveStaging(t, n, s)
	for _, c := range []struct{ name, query string }{
		{"no id", "object=runs/17/x"},
		{"nowhere to go", "id=rank-2"},
		{"a third acknowledgement", "id=rank-2&object=runs/17/x&ack=committed"},
	} {
		resp := post(t, srv, c.query, []byte("8 bytes."), "secret")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s got %s, want a bad request", c.name, resp.Status)
		}
		resp.Body.Close()
	}
}

func TestACheckpointWithoutASizeIsRefused(t *testing.T) {
	// Capacity is reserved before the first byte, so a reservation needs a
	// size. A chunked body would have the node discover it while writing,
	// which is the ordering RFC-0013 argues against.
	n, s := newNode(t, 1<<20), newStore()
	srv := serveStaging(t, n, s)

	pr, pw := io.Pipe()
	go func() { pw.Write([]byte("streaming with no length")); pw.Close() }()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/checkpoints?id=rank-3&object=runs/17/x", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusLengthRequired {
		t.Errorf("got %s, want a length to be required", resp.Status)
	}
}

func TestStagingWithoutTheTokenIsRefused(t *testing.T) {
	// Staging takes guaranteed capacity, so anything that could reach this
	// could promise the node's disk to itself and never give it back.
	n, s := newNode(t, 1<<20), newStore()
	srv := serveStaging(t, n, s)
	resp := post(t, srv, "id=rank-4&object=runs/17/x", []byte("8 bytes."), "not-the-token")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("got %s", resp.Status)
	}
	if len(n.holding()) != 0 {
		t.Error("capacity was reserved for an unauthorised request")
	}
}

func TestACheckpointThatSendsNoBytesHoldsNoCapacity(t *testing.T) {
	// Holding capacity is for a staged checkpoint that is the only copy of
	// itself. Nothing was staged here, so there is nothing to protect, and a
	// writer that declared a size and sent none would otherwise leave a lease
	// nobody will ever finish with.
	n, s := newNode(t, 1<<20), newStore()
	_, err := stager(t, n, s).Stage(t.Context(), request("rank-12", 4096), bytes.NewReader(nil))
	if !errors.Is(err, checkpoint.ErrNotStaged) {
		t.Fatalf("got %v, want nothing staged", err)
	}
	if held := n.holding(); len(held) != 0 {
		t.Errorf("the node is holding %v for a checkpoint that sent nothing", held)
	}
}
