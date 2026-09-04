package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mayur-tolexo/forebay/internal/agent"
	"github.com/mayur-tolexo/forebay/internal/dataserver"
	"github.com/mayur-tolexo/forebay/internal/fasttier"
	"github.com/mayur-tolexo/forebay/internal/lease"
	"github.com/mayur-tolexo/forebay/internal/metrics"
	"github.com/mayur-tolexo/forebay/internal/pool"
	"github.com/mayur-tolexo/forebay/internal/prefetch"
)

// selfLease names the lease an agent grants itself to hold the fast tier,
// standing in for a control plane. Elastic, so the pressure watch reclaims it
// like any other rather than exempting it.
const selfLease = "self-granted-tier"

// servingOptions is what the read path needs to exist.
type servingOptions struct {
	Socket     string
	Backend    backendOptions
	TierBytes  pool.Bytes
	BlockBytes int64
	FirstReads int
	// Metrics is where the read path publishes, and Ready is what decides
	// whether this node should be sent more work. Both come from the agent
	// rather than being made here, since the agent is what serves them.
	Metrics *metrics.Registry
	Ready   *metrics.Readiness
	// Prefetch turns on predicting what a reader will ask for next. Nil is
	// off, which is the default: RFC-0011's depth and accuracy floor are
	// guesses, and a prediction spends bandwidth on a node whose bandwidth
	// feeds an accelerator.
	Prefetch *prefetch.Config
}

// serving is a running read path.
type serving struct {
	stop func()
	tier *fasttier.Cache
	srv  *dataserver.Server
	// dropped counts cached blocks given up to reclamation.
	dropped *atomic.Int64
}

// Dropped reports how many cached blocks reclamation has taken back.
func (s *serving) Dropped() int64 { return s.dropped.Load() }

// Resident reports what the tier is holding, in bytes.
func (s *serving) Resident() int64 {
	_, _, blocks := s.tier.Stats()
	return int64(blocks) * s.tier.BlockSize()
}

// Saved reports what the tier saved against this node's own backend, and how
// much of its own hits that rests on.
func (s *serving) Saved() (time.Duration, float64) {
	e := s.srv.Efficiency()
	return e.Saved, e.CoveredFraction()
}

// serveReads joins the pieces that answer a read and starts listening.
func serveReads(a *agent.Agent, opts servingOptions) (*serving, error) {
	if opts.TierBytes <= 0 {
		return nil, fmt.Errorf("serving needs --tier-bytes, since a tier with no capacity holds nothing")
	}

	backend, err := openBackend(opts.Backend)
	if err != nil {
		return nil, err
	}

	tier, err := fasttier.New(fasttier.Config{
		BlockSize:      opts.BlockBytes,
		FirstReadLimit: opts.FirstReads,
	})
	if err != nil {
		return nil, err
	}

	// The tier's capacity is a lease like any other, so reclamation reaches
	// it. An agent granting its own is the stand-in; the lease is not.
	//
	// A lease outlives its process, so a restart replays this one and a second
	// grant is refused. Released first rather than adopted: a previous run may
	// have sized it differently, and what it holds is a cache.
	now := time.Now()
	switch _, err := a.Release(selfLease, now); {
	case err == nil, errors.Is(err, lease.ErrNoSuchLease), errors.Is(err, agent.ErrNoExtent):
		// A first run has nothing to release, which is those two errors.
	default:
		tier.Close()
		return nil, fmt.Errorf("releasing the tier a previous run left: %w", err)
	}
	if err := a.Grant(lease.Lease{
		ID: selfLease, Tenant: lease.NodeTenant, Class: lease.Elastic, Size: opts.TierBytes,
		GrantedAt: now, Term: 365 * 24 * time.Hour,
	}, now); err != nil {
		tier.Close()
		return nil, fmt.Errorf("granting the tier its capacity: %w", err)
	}
	extent, err := a.ExtentPath(selfLease)
	if err != nil {
		tier.Close()
		return nil, err
	}
	if err := tier.AddCapacity(selfLease, extent); err != nil {
		tier.Close()
		return nil, fmt.Errorf("giving the tier its extent: %w", err)
	}

	// The tier lets go before the extent is unlinked, or the blocks stay with
	// the descriptor and the workload never gets its disk back.
	//
	// Counted, not printed: this runs inside the window the reclaim deadline
	// is measured over, and a blocked stdout would be reported as the node
	// breaking its promise.
	var dropped atomic.Int64
	a.OnReleasing(func(leaseIDs []string) {
		for _, id := range leaseIDs {
			dropped.Add(int64(tier.Revoke(id)))
		}
	})

	// The name scopes what the tier holds, so two directories both called
	// "data" must not share it.
	name, err := opts.Backend.scope()
	if err != nil {
		tier.Close()
		return nil, fmt.Errorf("naming the backend: %w", err)
	}
	srv, err := dataserver.New(tier, backend, dataserver.Config{
		Backend: name, Metrics: opts.Metrics, Ready: opts.Ready,
		Prefetch: opts.Prefetch,
	})
	if err != nil {
		tier.Close()
		return nil, err
	}

	// Removed first, because a socket left behind by a previous run refuses
	// the bind and the previous run is gone either way.
	os.Remove(opts.Socket)
	l, err := net.Listen("unix", opts.Socket)
	if err != nil {
		tier.Close()
		return nil, fmt.Errorf("listening on %s: %w", opts.Socket, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.Serve(ctx, l); err != nil {
			fmt.Fprintln(os.Stderr, "forebay-agent: serving reads:", err)
		}
	}()

	fmt.Printf("answering reads on %s, %s of tier over %s, missing to %s\n",
		opts.Socket, opts.TierBytes, extent, describe(opts.Backend))
	fmt.Fprintln(os.Stderr, "the tier's capacity is a lease this agent granted itself, which a control plane would otherwise do")

	return &serving{
		tier: tier, srv: srv, dropped: &dropped,
		stop: func() {
			cancel()
			wg.Wait()
			tier.Close()
			os.Remove(opts.Socket)
		},
	}, nil
}
