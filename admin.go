package main

import (
	"context"
	_ "embed"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// adminHTML is the single-file management page, served at the admin port root.
//
//go:embed admin.html
var adminHTML []byte

// AdminHandler builds the router for the loopback-only management port. Every
// route is gated by loopbackOnly so that even if the listener is somehow bound
// beyond 127.0.0.1, only local callers are served — important because this
// surface is intended to grow more privileged admin operations over time.
func (s *Server) AdminHandler() http.Handler {
	r := chi.NewRouter()
	if s.logger != nil {
		r.Use(s.accessLog)
	}
	r.Use(loopbackOnly)
	r.Use(adminHostOnly)
	r.Get("/", s.handleAdminPage)
	r.Get("/health", s.handleHealth)
	r.Get("/api/status.json", s.handleAdminStatus)
	return r
}

// loopbackOnly rejects any request whose real TCP peer is not a loopback
// address. It trusts only RemoteAddr (the actual connection), never forwarded
// headers like X-Forwarded-For, which a client can spoof.
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackRemote(r.RemoteAddr) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "permission_error", "message": "admin endpoints are restricted to localhost"},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackRemote reports whether remoteAddr (host:port) is a loopback IP.
func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// adminHostOnly rejects requests whose Host header is not a loopback name. This
// defends against DNS-rebinding: even though loopbackOnly already requires a
// loopback TCP peer, a rebound attacker domain resolving to 127.0.0.1 would pass
// that check while the browser treats the response as same-origin. Pinning the
// Host to localhost closes that (and cross-origin CSRF) for the admin surface,
// which is expected to grow more privileged operations.
func adminHostOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "permission_error", "message": "admin endpoints require a localhost Host header"},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost reports whether the Host header (host or host:port) names the
// local machine: a loopback IP literal, "localhost", or a "localhost" subdomain.
func isLoopbackHost(hostHeader string) bool {
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	host = strings.ToLower(host)
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(adminHTML)
}

// handleAdminStatus returns the account, usage and model information rendered by
// the admin page.
func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()

	resp := map[string]any{
		"service": "kiro-anthropic",
		"version": version,
		"now":     now.UTC().Format(time.RFC3339),
		"account": s.accountView(ctx),
		"models":  s.modelsView(ctx, now),
	}

	// Usage is a live control-plane call (cached); surface failures inline so the
	// rest of the page still renders.
	if u, err := s.ensureUsage(ctx); err != nil {
		resp["usage"] = map[string]any{"error": err.Error()}
	} else {
		resp["usage"] = u
		// Fill the account email from usage when available.
		if acct, ok := resp["account"].(map[string]any); ok && u.Email != "" {
			acct["email"] = u.Email
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// accountView assembles the account/auth panel data from the token store.
func (s *Server) accountView(ctx context.Context) map[string]any {
	tok := s.store.snapshot()

	proxy := s.cfg.ProxyURL
	if proxy == "" {
		proxy = "direct (no proxy)"
	}

	acct := map[string]any{
		"provider":            tok.Provider,
		"auth_method":         tok.AuthMethod,
		"region":              s.store.region(),
		"token_file":          s.cfg.TokenFile,
		"access_token_masked": masked(tok.AccessToken),
		"outbound_proxy":      proxy,
		"api_key_required":    s.cfg.APIKey != "",
		"expires_at":          tok.ExpiresAt,
	}

	exp := tok.expiry()
	switch {
	case exp.IsZero():
		acct["expiry_state"] = "unknown"
	default:
		d := time.Until(exp).Round(time.Second)
		state := "valid"
		switch {
		case d <= 0:
			state = "expired"
		case d < tokenRefreshBuffer:
			state = "expiring soon"
		}
		acct["expiry_state"] = state
		acct["expires_in_seconds"] = int(d.Seconds())
	}

	// profileArn may need a network resolve for Enterprise; keep failures soft.
	if arn, err := s.store.ProfileArn(ctx); err != nil {
		acct["profile_arn_error"] = err.Error()
	} else {
		acct["profile_arn"] = arn
	}
	return acct
}

// modelsView renders the account's models as Anthropic ModelInfo objects, or an
// id-only fallback when the control plane is unreachable.
func (s *Server) modelsView(ctx context.Context, now time.Time) []map[string]any {
	ts := now.UTC().Format(time.RFC3339)
	out := make([]map[string]any, 0)
	if models := s.ensureModels(ctx); len(models) > 0 {
		for _, m := range models {
			out = append(out, modelInfoJSON(m, ts))
		}
		return out
	}
	for _, id := range fallbackModels {
		out = append(out, map[string]any{"type": "model", "id": id, "display_name": id, "created_at": ts})
	}
	return out
}
