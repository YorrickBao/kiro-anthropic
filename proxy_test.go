package main

import (
	"testing"
)

func TestProxyFunc(t *testing.T) {
	if proxyFunc("") != nil {
		t.Errorf(`proxyFunc("") should be nil (direct)`)
	}
	f := proxyFunc("http://127.0.0.1:7890")
	if f == nil {
		t.Fatalf("proxyFunc(valid) should return a resolver")
	}
	u, err := f(nil)
	if err != nil || u == nil || u.Host != "127.0.0.1:7890" {
		t.Errorf("resolver returned %v, %v", u, err)
	}
	if proxyFunc("://bad") != nil {
		t.Errorf("proxyFunc(invalid) should be nil")
	}
}

func TestConfigureProxy(t *testing.T) {
	// disable keywords -> direct ("")
	for _, kw := range []string{"none", "off", "direct", "NONE"} {
		cfg := &Config{ProxyURL: kw}
		configureProxy(cfg)
		if cfg.ProxyURL != "" {
			t.Errorf("keyword %q should disable proxy, got %q", kw, cfg.ProxyURL)
		}
	}

	// explicit value wins
	cfg := &Config{ProxyURL: "http://explicit:1234"}
	configureProxy(cfg)
	if cfg.ProxyURL != "http://explicit:1234" {
		t.Errorf("explicit proxy = %q", cfg.ProxyURL)
	}

	// empty + env set -> env value
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("HTTP_PROXY", "http://envproxy:8080")
	t.Setenv("http_proxy", "")
	cfg = &Config{ProxyURL: ""}
	configureProxy(cfg)
	if cfg.ProxyURL != "http://envproxy:8080" {
		t.Errorf("env proxy = %q", cfg.ProxyURL)
	}

	// empty + no env -> built-in default
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	cfg = &Config{ProxyURL: ""}
	configureProxy(cfg)
	if cfg.ProxyURL != defaultProxyURL {
		t.Errorf("default proxy = %q, want %q", cfg.ProxyURL, defaultProxyURL)
	}
}
