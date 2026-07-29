package main

import (
	"context"
	"log/slog"
	"time"
)

// depletedProbeInterval is how often the background probe re-checks accounts
// parked as depleted, so a reset/upgrade that restores credit is picked up
// without waiting on real traffic. It only touches depleted accounts, so the
// control-plane cost stays proportional to the number of exhausted accounts.
const depletedProbeInterval = 30 * time.Minute

// depletedProbe periodically re-fetches usage for accounts marked depleted and
// reconciles their state: an account with credit again is un-parked; one still
// exhausted is refined to its real reset_at. It mirrors accountRefresher's
// shape and shares the process-lifetime context from runServe.
type depletedProbe struct {
	srv      *Server
	logger   *slog.Logger  // optional; nil disables logging
	interval time.Duration // scan cadence; zero uses depletedProbeInterval
}

func newDepletedProbe(srv *Server, logger *slog.Logger) *depletedProbe {
	return &depletedProbe{srv: srv, logger: logger, interval: depletedProbeInterval}
}

// Run ticks until ctx is cancelled.
func (p *depletedProbe) Run(ctx context.Context) {
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

// scan re-checks every depleted account. A failed fetch leaves the account
// parked (the error may be transient); a successful one reconciles the depleted
// mark and refreshes the usage cache so the admin page benefits too.
func (p *depletedProbe) scan(ctx context.Context) {
	for _, id := range p.srv.selector.depletedIDs() {
		creds, ok := p.srv.selector.byID(id)
		if !ok {
			// Account vanished from the store; drop the stale mark.
			p.srv.selector.clearDepleted(id)
			continue
		}
		u, err := p.srv.kiro.GetUsage(ctx, creds)
		if err != nil {
			if p.logger != nil {
				p.logger.Log(context.Background(), slog.LevelDebug, "depleted probe usage fetch failed",
					"id", id, "error", err.Error())
			}
			continue
		}
		fetched := time.Now()
		// applyUsage reconciles the depleted mark (lifts it if credit returned,
		// refines to reset_at otherwise) without creating a new one, so a stale
		// snapshot can't re-park an account that recovered mid-fetch. Also
		// refresh the cache so the admin page shows the fresh figure.
		p.srv.selector.applyUsage(id, u, fetched, true)
		p.srv.usageMu.Lock()
		p.srv.usageCache[id] = usageCacheEntry{usage: u, fetched: fetched}
		p.srv.usageMu.Unlock()
		if u.Credit != nil && u.Credit.Remaining > 0 && p.logger != nil {
			p.logger.Log(context.Background(), slog.LevelInfo, "depleted account recovered",
				"id", id, "remaining", u.Credit.Remaining)
		}
	}
}
