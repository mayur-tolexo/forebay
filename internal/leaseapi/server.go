package leaseapi

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mayur-tolexo/forebay/internal/agent"
	"github.com/mayur-tolexo/forebay/internal/lease"
)

// Handler serves the lease protocol for one agent.
//
// Every route mutates or discloses, so every route needs the token. A node
// whose capacity anyone on the network can claim is a node anyone can fill,
// and the disk it fills belongs to the workload rather than to Forebay.
func Handler(a *agent.Agent, token string) http.Handler {
	s := &server{agent: a, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /leases", s.guard(s.propose))
	mux.HandleFunc("DELETE /leases/{id}", s.guard(s.release))
	mux.HandleFunc("GET /capacity", s.guard(s.capacity))
	return mux
}

type server struct {
	agent *agent.Agent
	token string
}

// guard refuses anything without the token.
//
// Compared in constant time, because a token compared with == leaks its prefix
// to anyone willing to time the answers, and this one grants disk.
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

// propose offers the node a lease, which it accepts only if its own accounting
// says the capacity exists.
func (s *server) propose(w http.ResponseWriter, r *http.Request) {
	var p Proposal
	body, err := readBounded(r)
	if err != nil {
		reply(w, http.StatusBadRequest, Decision{Reason: err.Error(), Refusal: Malformed})
		return
	}
	if err := decode(body, &p); err != nil {
		reply(w, http.StatusBadRequest, Decision{Reason: err.Error(), Refusal: Malformed})
		return
	}
	l, err := p.Lease()
	if err != nil {
		reply(w, http.StatusBadRequest, Decision{Reason: err.Error(), Refusal: Malformed})
		return
	}

	switch err := s.agent.Grant(l, time.Now()); {
	case err == nil:
		reply(w, http.StatusOK, Decision{Granted: true})
	case errors.Is(err, lease.ErrDuplicate):
		// A control plane that timed out and retried sends the same lease.
		// Telling it no would make a retry that worked look like a failure,
		// and the second proposal would be abandoned for a lease the node is
		// already holding.
		reply(w, http.StatusOK, Decision{Granted: true, Already: true})
	default:
		reply(w, http.StatusOK, Decision{Reason: err.Error(), Refusal: classify(err)})
	}
}

// classify sorts a refusal into the kinds a caller can act on. Anything else
// is reported as no capacity, which is the answer that makes a planner look
// elsewhere rather than wait.
func classify(err error) Refusal {
	switch {
	case errors.Is(err, lease.ErrChurning), errors.Is(err, lease.ErrCooldown):
		return BackingOff
	case errors.Is(err, lease.ErrTenantQuota), errors.Is(err, lease.ErrNoTenant):
		return OverQuota
	case errors.Is(err, lease.ErrBadClass), errors.Is(err, lease.ErrBadTerm):
		return Malformed
	default:
		return NoCapacity
	}
}

// release gives a lease back.
//
// A lease the node does not have is reported as done rather than as an error.
// The caller wanted it gone and it is gone, and a control plane retrying a
// release should not be left holding a failure it can never clear.
func (s *server) release(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch _, err := s.agent.Release(id, time.Now()); {
	case err == nil, errors.Is(err, lease.ErrNoSuchLease), errors.Is(err, agent.ErrNoExtent):
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// capacity answers what this node's pool holds, with when it was measured.
func (s *server) capacity(w http.ResponseWriter, r *http.Request) {
	acct := s.agent.Accounting()
	reply(w, http.StatusOK, Capacity{
		Bytes: int64(acct.Capacity), Reserved: int64(acct.Reserved),
		Borrowed: int64(acct.Borrowed), Free: int64(acct.Free()),
		MeasuredAt: time.Now(),
	})
}

// readBounded reads a request body, refusing one too large to be a proposal.
//
// A truncated body would fail to parse anyway, so this is not what stops a
// lease being granted for a size nobody sent. What it buys is the reason: an
// operator reading "a proposal larger than 65536 bytes is not one" knows what
// to fix, and one reading "unexpected end of JSON input" does not.
func readBounded(r *http.Request) ([]byte, error) {
	const max = 64 << 10
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("leaseapi: reading the proposal: %w", err)
	}
	if len(body) > max {
		return nil, fmt.Errorf("leaseapi: a proposal larger than %d bytes is not one", max)
	}
	return body, nil
}

// reply writes one answer.
func reply(w http.ResponseWriter, code int, v any) {
	body, err := encode(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(body)
}
