package checkpoint

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/mayur-tolexo/forebay/internal/pool"
)

// Handler serves staging for one node.
//
// On the agent's own surface and behind the same token as the leases, because
// staging takes guaranteed capacity: anything that could reach this could
// promise the node's disk to itself and never give it back.
//
// It has to live in the agent rather than in a command of its own. Reserving
// capacity means granting a lease, the agent holds the node lock that makes it
// the authority on that, and a second process could not grant one without
// taking the lock away from the thing doing the reclaiming.
func Handler(s *Stager, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /checkpoints", guard(token, func(w http.ResponseWriter, r *http.Request) {
		stage(s, w, r)
	}))
	return mux
}

// guard refuses anything without the token, compared in constant time.
func guard(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		given := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(given), []byte(token)) != 1 {
			http.Error(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// Outcome is what a writer is told.
type Outcome struct {
	// Ack is the word this acknowledgement is, so a writer logs the same one
	// the document defines.
	Ack string `json:"ack"`
	// Survives and Lost are what it withstands and what it does not, in the
	// document's own words: a fast acknowledgement that did not say what it
	// costs is the failure RFC-0013 opens with.
	Survives string `json:"survives"`
	Lost     string `json:"lost"`
	// Bytes is what was staged.
	Bytes int64 `json:"bytes"`
}

// stage reserves, writes and acknowledges one checkpoint.
func stage(s *Stager, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	// The size comes from Content-Length rather than from a parameter, so
	// there is one number and the body cannot disagree with it. A request
	// that does not carry one is refused: reserving happens before the first
	// byte, and a reservation needs a size.
	if r.ContentLength < 0 {
		http.Error(w, "a checkpoint has to say how large it is, and this request did not: "+
			"capacity is reserved before the first byte is written, so send a Content-Length rather than a chunked body",
			http.StatusLengthRequired)
		return
	}

	req := Request{
		ID:     q.Get("id"),
		Tenant: q.Get("tenant"),
		Object: q.Get("object"),
		Bytes:  pool.Bytes(r.ContentLength),
		Ack:    Ack(q.Get("ack")),
	}
	c, err := s.Stage(r.Context(), req, r.Body)
	if err != nil {
		http.Error(w, err.Error(), statusFor(err))
		return
	}

	survives, lost, _ := c.Ack().Survives()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Outcome{
		Ack: string(c.Ack()), Survives: survives, Lost: lost, Bytes: r.ContentLength,
	})
}

// statusFor maps a refusal to a status a writer can act on.
//
// The distinction that matters is whether writing straight through is the
// answer. A node that cannot promise the capacity is a conflict the writer
// works around; a request that names nothing is the writer's own error and no
// other node will take it either.
func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrTooLarge), errors.Is(err, ErrRevocable):
		// Not an error in the request. This node cannot stage it, and the
		// writer's answer is to write through, which is what it would have
		// done without this feature.
		return http.StatusConflict
	case errors.Is(err, ErrOverran):
		// The framework said one size and sent another, which RFC-0013 calls
		// its error and requires to be reported as one.
		return http.StatusBadRequest
	case errors.Is(err, ErrNotStaged), errors.Is(err, ErrRequest):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
