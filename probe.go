package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// depletedProbeInterval is how often the background worker reconciles quota
// classes. Conservative probes cover strict unknown/depleted accounts, while
// authoritative observations keep routable overage-enabled accounts current.
const (
	depletedProbeInterval    = 30 * time.Minute
	depletedProbeConcurrency = 4
	depletedProbeTimeout     = 15 * time.Second
)

// depletedProbe periodically re-fetches Usage for quota reconciliation targets.
// Conservative probes recover strict/depleted accounts only with positive Base
// credit; authoritative observations keep overage-enabled tiers current. It
// mirrors accountRefresher's shape and shares runServe's lifetime context.
type depletedProbe struct {
	srv         *Server
	logger      *slog.Logger  // optional; nil disables logging
	interval    time.Duration // scan cadence; zero uses depletedProbeInterval
	concurrency int           // simultaneous accounts; non-positive uses the default
	timeout     time.Duration // per-account wait; non-positive uses the default

	limiterMu    sync.Mutex
	fetchLimiter chan struct{}
}

func newDepletedProbe(srv *Server, logger *slog.Logger) *depletedProbe {
	return &depletedProbe{
		srv: srv, logger: logger, interval: depletedProbeInterval,
		concurrency: depletedProbeConcurrency, timeout: depletedProbeTimeout,
	}
}

// usageFetchLimiter is shared across scans and remains held until the detached
// singleflight fetch itself completes, not merely until one waiter times out.
func (p *depletedProbe) usageFetchLimiter(limit int) chan struct{} {
	p.limiterMu.Lock()
	defer p.limiterMu.Unlock()
	if p.fetchLimiter == nil {
		p.fetchLimiter = make(chan struct{}, limit)
	}
	return p.fetchLimiter
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

// scan re-checks every current reconciliation target with bounded workers and
// an independent timeout per account. A stalled account occupies only one
// worker; other workers continue, and a timeout/error leaves selector state
// unchanged.
func (p *depletedProbe) scan(ctx context.Context) {
	if p.srv == nil || p.srv.selector == nil {
		return
	}
	targets := p.srv.selector.reconcileTargets()
	if len(targets) == 0 {
		return
	}
	concurrency := p.concurrency
	if concurrency <= 0 {
		concurrency = depletedProbeConcurrency
	}
	// Size the cross-scan limiter from the configured ceiling, not this scan's
	// target count. Otherwise a small first scan permanently throttles later ones.
	fetchLimiter := p.usageFetchLimiter(concurrency)
	workers := concurrency
	if workers > len(targets) {
		workers = len(targets)
	}
	timeout := p.timeout
	if timeout <= 0 {
		timeout = depletedProbeTimeout
	}

	jobs := make(chan usageReconcileTarget, len(targets))
	for _, target := range targets {
		jobs <- target
	}
	close(jobs)

	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				select {
				case <-ctx.Done():
					return
				case target, ok := <-jobs:
					if !ok {
						return
					}
					p.reconcileAccount(ctx, target, timeout, fetchLimiter)
				}
			}
		}()
	}
	wg.Wait()
}

func (p *depletedProbe) reconcileAccount(ctx context.Context, target usageReconcileTarget, timeout time.Duration, fetchLimiter chan struct{}) {
	accountCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u, err := p.srv.refreshUsageTarget(accountCtx, target, fetchLimiter)
	if err != nil {
		if p.logger != nil {
			p.logger.Log(context.Background(), slog.LevelDebug, "quota reconciliation usage fetch failed",
				"id", target.stamp.id, "error", err.Error())
		}
		return
	}
	if target.priorQuota == quotaDepleted && u != nil && u.Credit != nil && u.Credit.Remaining > 0 && p.logger != nil {
		p.logger.Log(context.Background(), slog.LevelInfo, "depleted account recovered",
			"id", target.stamp.id, "remaining", u.Credit.Remaining)
	}
}
