package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// accountRefreshInterval is how often the background refresher scans the store.
const accountRefreshInterval = 60 * time.Second

// accountRefresher periodically refreshes stored accounts whose access token is
// at or near expiry, using each account's own SSO-OIDC client registration and
// refresh token. It operates only on the multi-account store, which is the sole
// source of accounts.
type accountRefresher struct {
	store    *AccountStore
	client   *http.Client
	logger   *slog.Logger  // optional; nil disables logging
	interval time.Duration // scan cadence; zero uses accountRefreshInterval
}

func newAccountRefresher(store *AccountStore, client *http.Client, logger *slog.Logger) *accountRefresher {
	return &accountRefresher{store: store, client: client, logger: logger, interval: accountRefreshInterval}
}

// Run scans on a ticker until ctx is cancelled. It performs one scan
// immediately so freshly-loaded stale accounts are handled at startup.
func (r *accountRefresher) Run(ctx context.Context) {
	interval := r.interval
	if interval <= 0 {
		interval = accountRefreshInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	r.scan(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.scan(ctx)
		}
	}
}

// scan refreshes every account due for renewal. Failures are logged and left
// for the next cycle; the refresh token usually remains valid so a transient
// error should not drop the account.
func (r *accountRefresher) scan(ctx context.Context) {
	now := time.Now()
	for _, a := range r.store.List() {
		if !accountNeedsRefresh(a, now) {
			continue
		}
		r.refreshOne(ctx, a)
	}
}

// refreshOne refreshes a single account and persists the new tokens. It routes
// through the store so a background refresh and a request-path refresh of the
// same account collapse into one CreateToken call (RefreshToken persists on
// success).
func (r *accountRefresher) refreshOne(ctx context.Context, a StoredAccount) {
	fresh, err := r.store.RefreshToken(ctx, r.client, a.ID)
	if err != nil {
		r.logf(slog.LevelWarn, "account token refresh failed", "id", a.ID, "label", a.Label, "error", err.Error())
		return
	}
	r.logf(slog.LevelInfo, "account token refreshed", "id", a.ID, "label", a.Label, "expires_at", fresh.ExpiresAt)
}

// accountNeedsRefresh reports whether an account should be refreshed now: it has
// a refresh token and its access token is missing, unparseable, expired, or
// within tokenRefreshBuffer of expiry.
func accountNeedsRefresh(a StoredAccount, now time.Time) bool {
	if a.RefreshToken == "" || a.ClientID == "" || a.ClientSecret == "" {
		return false
	}
	exp := a.expiry()
	if exp.IsZero() {
		// Unknown expiry with a usable refresh token: refresh to establish one.
		return true
	}
	return now.Add(tokenRefreshBuffer).After(exp)
}

func (r *accountRefresher) logf(level slog.Level, msg string, args ...any) {
	if r.logger != nil {
		r.logger.Log(context.Background(), level, msg, args...)
	}
}
