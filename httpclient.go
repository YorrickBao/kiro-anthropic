package main

import (
	"net/http"
	"net/url"
	"time"
)

// newHTTPClient builds an *http.Client used for ALL outbound calls to AWS
// (SSO-OIDC) and Kiro (runtime/management). A non-empty proxyURL routes every
// request through it; an empty proxyURL means a direct connection (the proxy
// decision, including any env vars, is resolved earlier by configureProxy).
//
// The client intentionally has no overall Timeout because GenerateAssistantResponse
// is a long-lived streaming call; per-attempt deadlines are applied by callers
// via context instead.
//
// HTTP/2 PING keepalive is enabled via Transport.HTTP2: when no frame is read
// from a connection for SendPingTimeout, the client sends a PING and closes the
// connection if no ACK arrives within PingTimeout. Kiro's GenerateAssistantResponse
// can sit silent for minutes (e.g. during extended thinking) and intermediate
// proxies/gateways with shorter idle timeouts otherwise reset the stream with
// INTERNAL_ERROR mid-response. The PING keeps the h2 connection alive across
// those silent gaps so the stream completes instead of being torn down.
func newHTTPClient(proxyURL string) *http.Client {
	transport := &http.Transport{
		Proxy:                 proxyFunc(proxyURL),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: 30 * time.Second,
			PingTimeout:     15 * time.Second,
		},
	}
	return &http.Client{Transport: transport}
}

// proxyFunc returns a proxy resolver for the transport. A non-empty URL is used
// for every request; an empty URL disables proxying (direct connection), and
// crucially does NOT fall back to environment variables, since the caller has
// already folded those into the resolved value.
func proxyFunc(proxyURL string) func(*http.Request) (*url.URL, error) {
	if proxyURL == "" {
		return nil // direct connection
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil
	}
	return func(*http.Request) (*url.URL, error) {
		return u, nil
	}
}
