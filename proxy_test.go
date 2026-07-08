package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProxyFunc(t *testing.T) {
	assert.Nil(t, proxyFunc(""), `proxyFunc("") should be nil (direct)`)

	f := proxyFunc("http://127.0.0.1:7890")
	require.NotNil(t, f, "proxyFunc(valid) should return a resolver")
	u, err := f(nil)
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.Equal(t, "127.0.0.1:7890", u.Host)

	assert.Nil(t, proxyFunc("://bad"), "proxyFunc(invalid) should be nil")
}

func TestConfigureProxy(t *testing.T) {
	// disable keywords -> direct ("")
	for _, kw := range []string{"none", "off", "direct", "NONE"} {
		cfg := &Config{ProxyURL: kw}
		require.NoError(t, configureProxy(cfg))
		assert.Emptyf(t, cfg.ProxyURL, "keyword %q should disable proxy", kw)
	}

	// explicit value wins
	cfg := &Config{ProxyURL: "http://explicit:1234"}
	require.NoError(t, configureProxy(cfg))
	assert.Equal(t, "http://explicit:1234", cfg.ProxyURL)

	// empty + env set -> env value
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("HTTP_PROXY", "http://envproxy:8080")
	t.Setenv("http_proxy", "")
	cfg = &Config{ProxyURL: ""}
	require.NoError(t, configureProxy(cfg))
	assert.Equal(t, "http://envproxy:8080", cfg.ProxyURL)

	// empty + no env -> built-in default
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	cfg = &Config{ProxyURL: ""}
	require.NoError(t, configureProxy(cfg))
	assert.Equal(t, defaultProxyURL, cfg.ProxyURL)
}
