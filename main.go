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
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
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
	Host       string
	Port       int
	ProxyURL   string // outbound proxy for calls to AWS / kiro.dev (e.g. http://127.0.0.1:7890)
	TokenFile  string // path to kiro-auth-token.json
	ProfileArn string // optional explicit profileArn override
	APIKey     string // optional key clients must send (x-api-key / Authorization)
	AgentMode  string // Kiro agent mode, e.g. "vibe"
	Region     string // optional region override (defaults to the token's region)
}

func defaultTokenFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".aws", "sso", "cache", "kiro-auth-token.json")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "serve":
		runServe(args)
	case "status":
		runStatus(args)
	case "models":
		runModels(args)
	case "version", "-v", "--version":
		fmt.Printf("kiro-anthropic %s\n", version)
		if commit != "" {
			fmt.Printf("commit: %s\n", commit)
		}
		if date != "" {
			fmt.Printf("built:  %s\n", date)
		}
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `kiro-anthropic - expose a Kiro account as an Anthropic-compatible API

USAGE:
  kiro-anthropic <command> [flags]

COMMANDS:
  serve      Start the Anthropic-compatible HTTP server (default port 17890)
  status     Show the current Kiro auth/token status
  models     List the model IDs available to this Kiro account
  version    Print version
  help       Show this help

Run "kiro-anthropic serve -h" for serve flags.
`)
}

// newServeFlags builds the flag set shared by "serve".
func newServeFlags() (*flag.FlagSet, *Config) {
	cfg := &Config{}
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.StringVar(&cfg.Host, "host", "127.0.0.1", "host/interface to bind")
	fs.IntVar(&cfg.Port, "port", 17890, "port to listen on")
	fs.StringVar(&cfg.ProxyURL, "proxy", "", "outbound HTTP proxy for AWS/Kiro calls; precedence: this flag > http(s)_proxy env > default "+defaultProxyURL+"; use 'none' to connect directly")
	fs.StringVar(&cfg.TokenFile, "token-file", defaultTokenFile(), "path to Kiro auth token JSON")
	fs.StringVar(&cfg.ProfileArn, "profile-arn", "", "explicit CodeWhisperer profileArn (auto-resolved if empty)")
	fs.StringVar(&cfg.APIKey, "api-key", "", "if set, clients must present this key via x-api-key or Authorization: Bearer")
	fs.StringVar(&cfg.AgentMode, "agent-mode", "vibe", "Kiro agent mode")
	fs.StringVar(&cfg.Region, "region", "", "region override (defaults to the token's region)")
	return fs, cfg
}

// configureProxy resolves the outbound proxy in place, with precedence:
//
//	--proxy flag  >  http(s)_proxy env  >  built-in default (defaultProxyURL)
//
// The special values none/off/direct disable proxying (direct connection).
func configureProxy(cfg *Config) {
	switch strings.ToLower(strings.TrimSpace(cfg.ProxyURL)) {
	case "none", "off", "direct", "false", "no":
		cfg.ProxyURL = "" // explicit direct connection
		return
	}
	if strings.TrimSpace(cfg.ProxyURL) == "" {
		if env := firstNonEmptyEnv("HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"); env != "" {
			cfg.ProxyURL = env
		} else {
			cfg.ProxyURL = defaultProxyURL
		}
	}
	if _, err := url.Parse(cfg.ProxyURL); err != nil {
		fatalf("invalid --proxy %q: %v", cfg.ProxyURL, err)
	}
}

func runServe(args []string) {
	fs, cfg := newServeFlags()
	_ = fs.Parse(args)

	configureProxy(cfg)

	client := newHTTPClient(cfg.ProxyURL)

	store, err := NewTokenStore(cfg, client)
	if err != nil {
		fatalf("auth init failed: %v", err)
	}

	srv := NewServer(cfg, store, client)

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		proxyNote := "direct (no proxy)"
		if cfg.ProxyURL != "" {
			proxyNote = cfg.ProxyURL
		}
		fmt.Printf("kiro-anthropic %s\n", version)
		fmt.Printf("  listening : http://%s\n", addr)
		fmt.Printf("  endpoint  : POST http://%s/v1/messages\n", addr)
		fmt.Printf("  outbound  : %s\n", proxyNote)
		fmt.Printf("  token     : %s\n", cfg.TokenFile)
		if cfg.APIKey == "" {
			fmt.Printf("  auth      : open (no api key required; bound to %s)\n", cfg.Host)
		} else {
			fmt.Printf("  auth      : x-api-key required\n")
		}
		fmt.Printf("  effort    : per request, default max (output_config.effort / reasoning_effort)\n")
		fmt.Printf("  max-tokens: per request, default max (caller max_tokens honored, clamped)\n")
		fmt.Println("  ready. press Ctrl+C to stop.")
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatalf("server error: %v", err)
		}
	}()

	<-ctx.Done()
	fmt.Println("\nshutting down...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

func runStatus(args []string) {
	fs, cfg := newServeFlags()
	fs.SetOutput(os.Stderr)
	// status shares the same flags; only a subset matters.
	_ = fs.Parse(args)

	configureProxy(cfg)
	client := newHTTPClient(cfg.ProxyURL)

	store, err := NewTokenStore(cfg, client)
	if err != nil {
		fatalf("auth init failed: %v", err)
	}

	tok := store.snapshot()
	proxyNote := cfg.ProxyURL
	if proxyNote == "" {
		proxyNote = "direct (no proxy)"
	}
	fmt.Printf("outbound   : %s\n", proxyNote)
	fmt.Printf("token file : %s\n", cfg.TokenFile)
	fmt.Printf("provider   : %s\n", orNone(tok.Provider))
	fmt.Printf("authMethod : %s\n", orNone(tok.AuthMethod))
	fmt.Printf("region     : %s\n", orNone(store.region()))
	fmt.Printf("expiresAt  : %s\n", orNone(tok.ExpiresAt))
	if tok.expiry().IsZero() {
		fmt.Printf("expiry     : unknown\n")
	} else {
		d := time.Until(tok.expiry()).Round(time.Second)
		state := "valid"
		if d <= 0 {
			state = "EXPIRED"
		} else if d < tokenRefreshBuffer {
			state = "expiring soon"
		}
		fmt.Printf("expiry     : %s (%s)\n", state, d)
	}

	fmt.Printf("access tok : %s\n", masked(tok.AccessToken))

	// Try to resolve the profileArn (may require a network call for Enterprise).
	arn, err := store.ProfileArn(context.Background())
	if err != nil {
		fmt.Printf("profileArn : (resolve failed: %v)\n", err)
	} else {
		fmt.Printf("profileArn : %s\n", orNone(arn))
	}
}

func runModels(args []string) {
	fs, cfg := newServeFlags()
	fs.SetOutput(os.Stderr)
	_ = fs.Parse(args)

	configureProxy(cfg)
	client := newHTTPClient(cfg.ProxyURL)

	store, err := NewTokenStore(cfg, client)
	if err != nil {
		fatalf("auth init failed: %v", err)
	}

	kc := NewKiroClient(cfg, store, client)
	models, err := kc.ListModels(context.Background())
	if err != nil {
		fatalf("list models failed: %v", err)
	}
	if len(models) == 0 {
		fmt.Println("(no models returned)")
		return
	}
	fmt.Printf("%d models available to this account:\n", len(models))
	for _, m := range models {
		def := ""
		if m.Default || m.IsDefault {
			def = "  (default)"
		}
		name := m.ModelName
		if name != "" && name != m.ModelID {
			name = "  " + name
		} else {
			name = ""
		}
		lim := ""
		if m.TokenLimits.MaxInputTokens > 0 || m.TokenLimits.MaxOutputTokens > 0 {
			lim = fmt.Sprintf("  [ctx %s in / %s out]",
				humanTokens(m.TokenLimits.MaxInputTokens), humanTokens(m.TokenLimits.MaxOutputTokens))
		}
		eff := ""
		if ec, ok := m.effort(); ok {
			eff = fmt.Sprintf("  [effort %s]", strings.Join(ec.Levels, "/"))
		}
		fmt.Printf("  %s%s%s%s%s\n", m.ModelID, name, lim, eff, def)
	}
	fmt.Println("\nUse any of these IDs as the Anthropic \"model\", or aliases like")
	fmt.Println("\"claude-opus-...\"/\"...sonnet...\" (mapped automatically), or \"auto\".")
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

// humanTokens formats a token count compactly, e.g. 1000000 -> "1M", 128000 -> "128K".
func humanTokens(n int) string {
	switch {
	case n <= 0:
		return "?"
	case n%1_000_000 == 0:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n%1000 == 0:
		return fmt.Sprintf("%dK", n/1000)
	case n >= 1000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
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

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	os.Exit(1)
}
