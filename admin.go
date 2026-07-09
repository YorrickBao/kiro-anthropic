package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
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
	r.Get("/api/accounts.json", s.handleAccountsList)
	r.Post("/api/login/start", s.handleLoginStart)
	r.Post("/api/accounts/delete", s.handleAccountDelete)
	r.Post("/api/accounts/label", s.handleAccountLabel)
	r.Post("/api/accounts/import", s.handleAccountImport)
	r.Get("/oauth/callback", s.handleLoginCallback)
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

// handleAccountsList returns the stored (self-managed) accounts, redacted.
func (s *Server) handleAccountsList(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil {
		writeJSON(w, http.StatusOK, map[string]any{"accounts": []any{}})
		return
	}
	list := s.accounts.List()
	views := make([]map[string]any, 0, len(list))
	for _, a := range list {
		views = append(views, a.view())
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": views})
}

// handleLoginStart begins an IdC authorization-code sign-in. It registers an
// OIDC client and returns an authorize URL for the user's browser. The
// redirect_uri points back to this admin origin's /oauth/callback; the
// adminHostOnly middleware guarantees that origin is a loopback name.
func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil || s.login == nil {
		adminError(w, http.StatusServiceUnavailable, "account store is not configured")
		return
	}
	var body struct {
		StartURL string `json:"start_url"`
		Region   string `json:"region"`
		Label    string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		adminError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}

	redirectURI := fmt.Sprintf("http://%s/oauth/callback", r.Host)
	authorizeURL, _, err := s.login.startLogin(r.Context(), body.StartURL, body.Region, body.Label, redirectURI)
	if err != nil {
		noteError(r.Context(), err.Error())
		adminError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authorize_url": authorizeURL})
}

// handleLoginCallback receives the browser redirect from AWS, exchanges the
// authorization code for tokens, persists the account, and renders a small
// HTML page telling the user to return to the admin page.
func (s *Server) handleLoginCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		desc := q.Get("error_description")
		renderCallbackPage(w, false, fmt.Sprintf("Authorization failed: %s %s", e, desc))
		return
	}
	state := q.Get("state")
	code := q.Get("code")
	if s.accounts == nil || s.login == nil {
		renderCallbackPage(w, false, "Account store is not configured.")
		return
	}

	acct, err := s.login.completeLogin(r.Context(), state, code)
	if err != nil {
		noteError(r.Context(), err.Error())
		renderCallbackPage(w, false, "Sign-in failed: "+err.Error())
		return
	}
	if err := s.accounts.Add(acct); err != nil {
		noteError(r.Context(), err.Error())
		renderCallbackPage(w, false, "Signed in but could not save the account: "+err.Error())
		return
	}
	renderCallbackPage(w, true, "Signed in successfully. You can close this window and return to the admin page.")
}

// handleAccountImport imports the credentials from the local Kiro auth cache
// (the --token-file and its client registration) into the multi-account store.
// A credential already present (same clientId + refreshToken) is not duplicated.
func (s *Server) handleAccountImport(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil {
		adminError(w, http.StatusServiceUnavailable, "account store is not configured")
		return
	}
	acct, err := importLocalCredentials(r.Context(), s.login.client, s.cfg.TokenFile)
	if err != nil {
		noteError(r.Context(), err.Error())
		adminError(w, http.StatusBadRequest, err.Error())
		return
	}
	if id, ok := s.accounts.FindByRefreshToken(acct.ClientID, acct.RefreshToken); ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "already_present": true})
		return
	}
	if err := s.accounts.Add(acct); err != nil {
		adminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": acct.ID, "already_present": false})
}

// handleAccountLabel updates the note (label) of a stored account.
func (s *Server) handleAccountLabel(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil {
		adminError(w, http.StatusServiceUnavailable, "account store is not configured")
		return
	}
	var body struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		adminError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if body.ID == "" {
		adminError(w, http.StatusBadRequest, "id is required")
		return
	}
	if err := s.accounts.UpdateLabel(body.ID, strings.TrimSpace(body.Label)); err != nil {
		adminError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAccountDelete removes a stored account by id.
func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil {
		adminError(w, http.StatusServiceUnavailable, "account store is not configured")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		adminError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if body.ID == "" {
		adminError(w, http.StatusBadRequest, "id is required")
		return
	}
	if err := s.accounts.Remove(body.ID); err != nil {
		adminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminError writes an error JSON in the shape the admin page expects.
func adminError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"message": msg},
	})
}

// renderCallbackPage writes the minimal OAuth callback landing page.
func renderCallbackPage(w http.ResponseWriter, ok bool, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	status := http.StatusOK
	if !ok {
		status = http.StatusBadRequest
	}
	w.WriteHeader(status)
	title := "Sign-in complete"
	if !ok {
		title = "Sign-in failed"
	}
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>%s</title>
<style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;color:#1a1a1a}
h1{font-size:1.25rem}.ok{color:#0a7d33}.err{color:#b00020}</style></head>
<body><h1 class="%s">%s</h1><p>%s</p></body></html>`,
		htmlEscape(title), map[bool]string{true: "ok", false: "err"}[ok], htmlEscape(title), htmlEscape(msg))
}

// htmlEscape is a tiny escaper for the text we interpolate into the callback page.
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
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
