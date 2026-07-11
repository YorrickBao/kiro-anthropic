package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
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
	r.Get("/api/update.json", s.handleUpdateCheck)
	r.Get("/api/accounts.json", s.handleAccountsList)
	r.Post("/api/login/start", s.handleLoginStart)
	r.Post("/api/accounts/delete", s.handleAccountDelete)
	r.Post("/api/accounts/label", s.handleAccountLabel)
	r.Post("/api/accounts/import", s.handleAccountImport)
	r.Get("/api/accounts/export", s.handleAccountExport)
	r.Post("/api/accounts/import-bundle", s.handleAccountImportBundle)
	r.Post("/api/accounts/refresh", s.handleAccountRefresh)
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

// handleAdminStatus returns the per-account identity, usage and model
// information rendered by the admin page. Every stored account is included;
// usage is fetched per-account (cached) and surfaced inline so one account's
// failure does not hide the rest.
func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()

	proxy := s.cfg.ProxyURL
	if proxy == "" {
		proxy = "direct (no proxy)"
	}

	resp := map[string]any{
		"service":          "kiro-anthropic",
		"version":          version,
		"now":              now.UTC().Format(time.RFC3339),
		"outbound_proxy":   proxy,
		"api_key_required": s.cfg.APIKey != "",
		"accounts":         s.accountsStatus(ctx, now),
		"models":           s.modelsView(ctx, now),
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUpdateCheck returns the version-update view (current version, latest
// release, and aggregated notes of newer releases). A GitHub failure degrades
// softly: it returns 200 with update_available=false and an error string so the
// admin page can stay silent rather than surfacing a scary error.
func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	st, err := s.ensureUpdateStatus(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"current":          version,
			"update_available": false,
			"releases":         []any{},
			"error":            err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// accountsStatus assembles the per-account panel data: identity (redacted) plus
// live usage (cached per account). Failures are surfaced inline per account.
func (s *Server) accountsStatus(ctx context.Context, now time.Time) []map[string]any {
	out := make([]map[string]any, 0)
	if s.selector == nil {
		return out
	}
	for _, creds := range s.selector.listAll() {
		v := creds.acct.view()
		if u, err := s.ensureUsage(ctx, creds); err != nil {
			v["usage"] = map[string]any{"error": err.Error()}
		} else {
			v["usage"] = u
			if u.Email != "" && (v["email"] == nil || v["email"] == "") {
				v["email"] = u.Email
			}
		}
		out = append(out, v)
	}
	return out
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
	// If this AWS account is already stored (e.g. previously imported), refresh
	// its credentials in place instead of adding a duplicate.
	saveErr := error(nil)
	if id, ok := s.accounts.FindDuplicate(*acct); ok {
		saveErr = s.accounts.ReplaceCredentials(id, acct)
	} else {
		saveErr = s.accounts.Add(acct)
	}
	if saveErr != nil {
		noteError(r.Context(), saveErr.Error())
		renderCallbackPage(w, false, "Signed in but could not save the account: "+saveErr.Error())
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
	if id, ok := s.accounts.FindDuplicate(*acct); ok {
		// Same account already stored (possibly added via sign-in): refresh its
		// credentials in place rather than creating a duplicate.
		if err := s.accounts.ReplaceCredentials(id, acct); err != nil {
			adminError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "already_present": true})
		return
	}
	if err := s.accounts.Add(acct); err != nil {
		adminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": acct.ID, "already_present": false})
}

// maxImportBundleBytes caps the accepted import upload. A pool of a few hundred
// accounts is well under this; the limit just bounds memory for a hostile body.
const maxImportBundleBytes = 16 << 20 // 16 MiB

// handleAccountExport streams the entire account pool as a JSON bundle for
// migration to another server. Unlike the redacted status/list endpoints, this
// deliberately includes the long-lived secrets (refreshToken, clientSecret) so
// the pool can be reconstituted elsewhere. Exposing them here is acceptable only
// because the admin surface is loopback-only and Host-pinned (see AdminHandler);
// the downloaded file must still be treated as sensitive. The bundle re-imports
// via handleAccountImportBundle.
func (s *Server) handleAccountExport(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil {
		adminError(w, http.StatusServiceUnavailable, "account store is not configured")
		return
	}
	bundle := map[string]any{
		"type":        "kiro-anthropic/accounts-export",
		"version":     1,
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"accounts":    s.accounts.List(),
	}
	filename := fmt.Sprintf("kiro-anthropic-accounts-%s.json", time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(bundle)
}

// handleAccountImportBundle merges an exported bundle (see handleAccountExport)
// into the store. It accepts both the wrapped export shape and the bare
// on-disk {"accounts":[...]} layout, since both carry an "accounts" array.
// Accounts already present (matched by identity) are refreshed in place; the
// rest are added. The response reports how many were added vs replaced.
func (s *Server) handleAccountImportBundle(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil {
		adminError(w, http.StatusServiceUnavailable, "account store is not configured")
		return
	}
	var bundle struct {
		Accounts []*StoredAccount `json:"accounts"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxImportBundleBytes)).Decode(&bundle); err != nil {
		adminError(w, http.StatusBadRequest, "invalid JSON bundle: "+err.Error())
		return
	}
	if len(bundle.Accounts) == 0 {
		adminError(w, http.StatusBadRequest, "bundle contains no accounts")
		return
	}
	res, err := s.accounts.ImportAccounts(bundle.Accounts)
	if err != nil {
		noteError(r.Context(), err.Error())
		adminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "added": res.Added, "replaced": res.Replaced, "total": len(bundle.Accounts),
	})
}

// handleAccountRefresh forces an SSO-OIDC token refresh for one account (when
// "id" is given) or every stored account (when "id" is empty). Refreshes are
// coordinated by the store's singleflight, so this cannot double-spend a
// rotating refresh token even if the background refresher fires concurrently.
// The per-account usage cache is invalidated on success so the panel reflects
// the fresh token on the next status poll. Per-account failures are reported
// inline; the response is still 200 unless a specific id is unknown.
func (s *Server) handleAccountRefresh(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil {
		adminError(w, http.StatusServiceUnavailable, "account store is not configured")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	// An empty body is allowed and means "refresh all".
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	ctx := r.Context()
	var ids []string
	if id := strings.TrimSpace(body.ID); id != "" {
		if _, ok := s.accounts.Get(id); !ok {
			adminError(w, http.StatusNotFound, "account not found: "+id)
			return
		}
		ids = []string{id}
	} else {
		for _, a := range s.accounts.List() {
			ids = append(ids, a.ID)
		}
	}

	results := make([]map[string]any, 0, len(ids))
	refreshed := 0
	for _, id := range ids {
		fresh, err := s.accounts.RefreshToken(ctx, s.login.client, id)
		if err != nil {
			noteError(ctx, err.Error())
			results = append(results, map[string]any{"id": id, "ok": false, "error": err.Error()})
			continue
		}
		s.invalidateUsage(id)
		refreshed++
		results = append(results, map[string]any{"id": id, "ok": true, "expires_at": fresh.ExpiresAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "refreshed": refreshed, "total": len(ids), "results": results,
	})
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

// modelsView renders the available models (from any account) as Anthropic
// ModelInfo objects, or an id-only fallback when no account can list models.
func (s *Server) modelsView(ctx context.Context, now time.Time) []map[string]any {
	ts := now.UTC().Format(time.RFC3339)
	out := make([]map[string]any, 0)
	if models := s.anyModels(ctx); len(models) > 0 {
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
