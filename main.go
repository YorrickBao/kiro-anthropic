// Command kiro-anthropic runs a local HTTP service that exposes a Kiro
// (Amazon Q Developer / CodeWhisperer) account through the Anthropic Messages
// API protocol, so any Anthropic-compatible client can talk to Kiro's Claude
// models.
//
// All request/response shapes, endpoints and the auth flow were derived from
// Kiro's own bundled client and the official AWS SSO-OIDC / Anthropic docs.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// Build metadata. version/commit/date are overridden at build time via
// -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "0.1.0"
	commit  = ""
	date    = ""
)

// defaultProxyURL is used when neither --proxy nor an http(s)_proxy env var is set.
const defaultProxyURL = "http://127.0.0.1:7890"

// Config holds everything the server needs to run.
type Config struct {
	Host          string
	Port          int
	AdminPort     int    // loopback-only management port (default 27890)
	ProxyURL      string // outbound proxy for calls to AWS / kiro.dev (e.g. http://127.0.0.1:7890)
	TokenFile     string // path to kiro-auth-token.json (source for startup import)
	AccountsFile  string // path to the multi-account credential store
	NoImportLocal bool   // if set, do not auto-import local Kiro credentials on startup
	APIKey        string // optional key clients must send (x-api-key / Authorization)
	AgentMode     string // Kiro agent mode, e.g. "vibe"
	Log           bool   // enable request logging (off by default); logs to stdout unless LogFile is set
	LogFile       string // if set, write the request log here instead of stdout ("none"/"off" disables)
}

func defaultTokenFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".aws", "sso", "cache", "kiro-auth-token.json")
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// newRootCmd builds the cobra command tree.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "kiro-anthropic",
		Short: "expose a Kiro account as an Anthropic-compatible API",
		Long: "kiro-anthropic - expose a Kiro account as an Anthropic-compatible API\n\n" +
			"Proxies a Kiro (Amazon Q Developer / CodeWhisperer) account behind the\n" +
			"Anthropic Messages API so any Anthropic-compatible client can use it.",
		// We print errors ourselves in main and don't want usage spam on
		// every runtime error.
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	root.SetVersionTemplate(versionString())
	root.AddCommand(
		newServeCmd(),
		newVersionCmd(),
		newUpgradeCmd(),
	)
	return root
}

// versionString renders the multi-line version banner shared by the "version"
// subcommand and the --version flag.
func versionString() string {
	s := fmt.Sprintf("kiro-anthropic %s\n", version)
	if commit != "" {
		s += fmt.Sprintf("commit: %s\n", commit)
	}
	if date != "" {
		s += fmt.Sprintf("built:  %s\n", date)
	}
	return s
}

// addServerFlags registers the flags for the serve command.
func addServerFlags(cmd *cobra.Command, cfg *Config) {
	f := cmd.Flags()
	f.StringVar(&cfg.Host, "host", "127.0.0.1", "host/interface to bind")
	f.IntVar(&cfg.Port, "port", 17890, "port to listen on")
	f.IntVar(&cfg.AdminPort, "admin-port", 27890, "loopback-only management port (auto-increments if in use)")
	f.StringVar(&cfg.ProxyURL, "proxy", "", "outbound HTTP proxy for AWS/Kiro calls; precedence: this flag > http(s)_proxy env > default "+defaultProxyURL+"; use 'none' to connect directly")
	f.StringVar(&cfg.TokenFile, "token-file", defaultTokenFile(), "path to Kiro auth token JSON")
	f.StringVar(&cfg.AccountsFile, "accounts-file", defaultAccountsFile(), "path to the multi-account credential store")
	f.BoolVar(&cfg.NoImportLocal, "no-import-local", false, "do not auto-import the local Kiro credentials (--token-file) into the account pool on startup")
	f.StringVar(&cfg.APIKey, "api-key", "", "if set, clients must present this key via x-api-key or Authorization: Bearer")
	f.StringVar(&cfg.AgentMode, "agent-mode", "vibe", "Kiro agent mode")
	f.BoolVar(&cfg.Log, "log", false, "enable request logging to stdout (the window); off by default")
	f.StringVar(&cfg.LogFile, "log-file", "", "write the request log to this file instead of stdout (implies --log); 'none' disables")
}

func newServeCmd() *cobra.Command {
	cfg := &Config{}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the Anthropic-compatible HTTP server (default port 17890)",
		Args:  cobra.NoArgs,
		RunE:  func(_ *cobra.Command, _ []string) error { return runServe(cfg) },
	}
	addServerFlags(cmd, cfg)
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Args:  cobra.NoArgs,
		Run:   func(_ *cobra.Command, _ []string) { fmt.Print(versionString()) },
	}
}

// configureProxy resolves the outbound proxy in place, with precedence:
//
//	--proxy flag  >  http(s)_proxy env  >  built-in default (defaultProxyURL)
//
// The special values none/off/direct disable proxying (direct connection).
// validateBindSecurity rejects an insecure exposure: binding the API to a
// non-loopback address without an api-key would serve the Anthropic endpoint,
// and thus the account pool quota, to anyone who can reach the port. A loopback
// bind (the default 127.0.0.1) is unaffected. The admin port is always
// loopback-only and is not considered here.
func validateBindSecurity(cfg *Config) error {
	if isLoopbackHost(cfg.Host) {
		return nil
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return fmt.Errorf("--host %q is non-loopback; --api-key is required to avoid exposing the account pool to the network (keep the default 127.0.0.1 and use an SSH tunnel, or set --api-key)", cfg.Host)
	}
	return nil
}

func configureProxy(cfg *Config) error {
	switch strings.ToLower(strings.TrimSpace(cfg.ProxyURL)) {
	case "none", "off", "direct", "false", "no":
		cfg.ProxyURL = "" // explicit direct connection
		return nil
	}
	if strings.TrimSpace(cfg.ProxyURL) == "" {
		if env := firstNonEmptyEnv("HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"); env != "" {
			cfg.ProxyURL = env
		} else {
			cfg.ProxyURL = defaultProxyURL
		}
	}
	if _, err := url.Parse(cfg.ProxyURL); err != nil {
		return fmt.Errorf("invalid --proxy %q: %w", cfg.ProxyURL, err)
	}
	return nil
}

// setupRequestLog resolves the --log / --log-file flags into a structured
// (slog) request logger. Logging is OFF unless --log is set or --log-file names
// a destination:
//
//	--log=false, --log-file=""  -> disabled (nil logger, no access log)
//	--log,       --log-file=""  -> stdout (the window)
//	--log-file="stdout"/"-"     -> stdout
//	--log-file="stderr"         -> stderr
//	--log-file="none"/"off"     -> disabled
//	--log-file=<path>           -> append to the file at <path>
//
// It returns the logger (nil when disabled), an optional closer for an opened
// file, a short note for the startup banner, and any file-open error.
func setupRequestLog(enable bool, file string) (*slog.Logger, func(), string, error) {
	newLogger := func(w io.Writer) *slog.Logger {
		return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	switch strings.ToLower(strings.TrimSpace(file)) {
	case "none", "off", "false", "no", "disabled":
		return nil, nil, "disabled", nil
	case "":
		if !enable {
			return nil, nil, "disabled", nil
		}
		return newLogger(os.Stdout), nil, "stdout (window)", nil
	case "stdout", "-":
		return newLogger(os.Stdout), nil, "stdout (window)", nil
	case "stderr":
		return newLogger(os.Stderr), nil, "stderr", nil
	default:
		f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, "", err
		}
		return newLogger(f), func() { _ = f.Close() }, file, nil
	}
}

func runServe(cfg *Config) error {
	if err := configureProxy(cfg); err != nil {
		return err
	}

	if err := validateBindSecurity(cfg); err != nil {
		return err
	}

	client := newHTTPClient(cfg.ProxyURL)

	srv := NewServer(cfg, client)

	accounts, err := NewAccountStore(cfg.AccountsFile)
	if err != nil {
		return fmt.Errorf("account store init failed: %w", err)
	}
	srv.setAccounts(accounts, client)

	// Unless disabled, adopt the local Kiro desktop credentials into the pool on
	// startup so an existing sign-in works out of the box. Best-effort: a missing
	// or unreadable cache is not fatal (the pool can still be filled via the
	// admin page), keeping zero-account startup possible.
	if !cfg.NoImportLocal {
		if n, err := importLocalIntoStore(context.Background(), accounts, client, cfg.TokenFile); err != nil {
			fmt.Fprintf(os.Stderr, "note: could not import local credentials (%v); pool starts with %d account(s)\n", err, len(accounts.List()))
		} else if n > 0 {
			fmt.Printf("  imported local Kiro credentials into the account pool\n")
		}
	}

	// One service-lifetime context stops refreshers, probes, and detached cache
	// warmups when SIGINT/SIGTERM begins graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv.setWarmupContext(ctx)

	// Pre-warm models and usage caches for all usable accounts so the first
	// request does not pay the control-plane fetch latency.
	srv.warmAllAccounts()

	logger, closeLog, logNote, err := setupRequestLog(cfg.Log, cfg.LogFile)
	if err != nil {
		return fmt.Errorf("could not open --log-file %q: %w", cfg.LogFile, err)
	}
	if closeLog != nil {
		defer closeLog()
	}
	srv.logger = logger

	// Bind the API listener first, then the admin listener. Both auto-increment
	// their port if it is already in use; binding the API first means the admin
	// listener naturally skips past it. The admin port is locked to loopback.
	const adminHost = "127.0.0.1"

	apiLn, apiPort, err := listenWithAutoIncrement(cfg.Host, cfg.Port)
	if err != nil {
		return fmt.Errorf("bind API port: %w", err)
	}
	cfg.Port = apiPort
	addr := fmt.Sprintf("%s:%d", cfg.Host, apiPort)

	adminLn, adminPort, err := listenWithAutoIncrement(adminHost, cfg.AdminPort)
	if err != nil {
		_ = apiLn.Close()
		return fmt.Errorf("bind admin port: %w", err)
	}
	cfg.AdminPort = adminPort
	adminAddr := fmt.Sprintf("%s:%d", adminHost, adminPort)

	httpServer := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 30 * time.Second}
	adminServer := &http.Server{Handler: srv.AdminHandler(), ReadHeaderTimeout: 30 * time.Second}

	// Periodically refresh stored accounts before their tokens expire. Stops
	// when ctx is cancelled.
	go newAccountRefresher(srv.accounts, client, logger).Run(ctx)

	// Periodically re-check depleted accounts so a reset/upgrade that restores
	// credit is picked up without waiting on real traffic. Stops when ctx is
	// cancelled.
	go newDepletedProbe(srv, logger).Run(ctx)

	serveErr := make(chan error, 2)
	go func() {
		if err := httpServer.Serve(apiLn); err != nil && err != http.ErrServerClosed {
			serveErr <- fmt.Errorf("server error: %w", err)
		}
	}()
	go func() {
		if err := adminServer.Serve(adminLn); err != nil && err != http.ErrServerClosed {
			serveErr <- fmt.Errorf("admin server error: %w", err)
		}
	}()

	proxyNote := "direct (no proxy)"
	if cfg.ProxyURL != "" {
		proxyNote = cfg.ProxyURL
	}
	fmt.Printf("kiro-anthropic %s\n", version)
	fmt.Printf("  listening : http://%s\n", addr)
	fmt.Printf("  endpoint  : POST http://%s/v1/messages\n", addr)
	fmt.Printf("  admin     : http://%s  (localhost only)\n", adminAddr)
	fmt.Printf("  outbound  : %s\n", proxyNote)
	fmt.Printf("  accounts  : %d in pool (%s)\n", len(accounts.List()), cfg.AccountsFile)
	if len(accounts.List()) == 0 {
		fmt.Printf("  ! no accounts available — sign in or import one via the admin page:\n")
		fmt.Printf("    http://%s\n", adminAddr)
	}
	if cfg.APIKey == "" {
		fmt.Printf("  auth      : open (no api key required; bound to %s)\n", cfg.Host)
	} else {
		fmt.Printf("  auth      : x-api-key required\n")
	}
	fmt.Printf("  effort    : per request, default max (output_config.effort / reasoning_effort)\n")
	fmt.Printf("  max-tokens: passed through to Kiro (backend does not enforce it)\n")
	fmt.Printf("  log       : %s\n", logNote)
	fmt.Println("  ready. press Ctrl+C to stop.")

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	fmt.Println("\nshutting down... (draining in-flight requests; press Ctrl+C again to force quit)")

	// Drain until all connections close on their own — no fixed deadline, so a
	// long-lived stream is allowed to finish rather than being cut at 5s. A
	// second SIGINT/SIGTERM cancels the wait and forces an immediate exit, which
	// is the escape hatch when a stream never ends.
	shutdownCtx, stopForce := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopForce()

	// Shut both servers down concurrently; each Shutdown returns once its
	// connections have drained (or shutdownCtx is cancelled by a second signal).
	var wg sync.WaitGroup
	for _, hs := range []*http.Server{httpServer, adminServer} {
		wg.Add(1)
		go func(hs *http.Server) {
			defer wg.Done()
			_ = hs.Shutdown(shutdownCtx)
		}(hs)
	}
	wg.Wait()
	return nil
}

// firstNonEmptyEnv returns the first non-empty environment variable value.
func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func masked(s string) string {
	if s == "" {
		return "(none)"
	}
	if len(s) <= 12 {
		return "****"
	}
	return s[:6] + "..." + s[len(s)-4:]
}
