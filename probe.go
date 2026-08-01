package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// depletedProbeInterval is how often the background probe re-checks accounts
// that still need a control-plane quota decision: strict unknown accounts and
// every account parked as depleted. This picks up reset/upgrade recovery without
// sending inference traffic; bounded workers cap the control-plane load.
const (
	depletedProbeInterval    = 30 * time.Minute
	depletedProbeConcurrency = 4
	depletedProbeTimeout     = 15 * time.Second
)

// depletedProbe periodically re-fetches usage for selector probe targets and
// reconciles their state: an account with base credit again is un-parked; one
// still exhausted remains blocked. It mirrors accountRefresher's shape and
// shares the process-lifetime context from runServe.
type depletedProbe struct {
	srv         *Server
	logger      *slog.Logger  // optional; nil disables logging
	interval    time.Duration // scan cadence; zero uses depletedProbeInterval
	concurrency int           // simultaneous accounts; non-positive uses the default
	timeout     time.Duration // per-account wait; non-positive uses the default
}

func newDepletedProbe(srv *Server, logger *slog.Logger) *depletedProbe {
	return &depletedProbe{
		srv: srv, logger: logger, interval: depletedProbeInterval,
		concurrency: depletedProbeConcurrency, timeout: depletedProbeTimeout,
	}
}

// Run performs an immediate scan, then repeats until ctx is cancelled.
func (p *depletedProbe) Run(ctx context.Context) {
	p.scan(ctx)
	if ctx.Err() != nil {
		return
	}
	interval := p.interval
	if interval <= 0 {
		interval = depletedProbeInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.scan(ctx)
		}
	}
}

// scan re-checks every current probe target with bounded workers and an
// independent timeout per account. A stalled account occupies only one worker;
// other workers continue, and a timeout/error leaves selector state unchanged.
func (p *depletedProbe) scan(ctx context.Context) {
	if p.srv == nil || p.srv.selector == nil {
		return
	}
	ids := p.srv.selector.probeIDs()
	if len(ids) == 0 {
		return
	}
	concurrency := p.concurrency
	if concurrency <= 0 {
		concurrency = depletedProbeConcurrency
	}
	if concurrency > len(ids) {
		concurrency = len(ids)
	}
	timeout := p.timeout
	if timeout <= 0 {
		timeout = depletedProbeTimeout
	}

	jobs := make(chan string, len(ids))
	for _, id := range ids {
		jobs <- id
	}
	close(jobs)

	var wg sync.WaitGroup
	wg.Add(concurrency)
	for range concurrency {
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				select {
				case <-ctx.Done():
					return
				case id, ok := <-jobs:
					if !ok {
						return
					}
					p.probeAccount(ctx, id, timeout)
				}
			}
		}()
	}
	wg.Wait()
}

func (p *depletedProbe) probeAccount(ctx context.Context, id string, timeout time.Duration) {
	accountCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u, err := p.srv.refreshUsage(accountCtx, id, usageObservationProbe)
	if err != nil {
		if p.logger != nil {
			p.logger.Log(context.Background(), slog.LevelDebug, "depleted probe usage fetch failed",
				"id", id, "error", err.Error())
		}
		return
	}
	if u.Credit != nil && u.Credit.Remaining > 0 && p.logger != nil {
		p.logger.Log(context.Background(), slog.LevelInfo, "depleted account recovered",
			"id", id, "remaining", u.Credit.Remaining)
	}
}
