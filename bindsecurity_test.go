package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateBindSecurity(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		apiKey  string
		wantErr bool
	}{
		// Loopback binds never require a key.
		{"loopback ip no key", "127.0.0.1", "", false},
		{"loopback ip with key", "127.0.0.1", "k", false},
		{"localhost name no key", "localhost", "", false},
		{"ipv6 loopback no key", "::1", "", false},

		// Non-loopback binds require a key.
		{"all interfaces no key", "0.0.0.0", "", true},
		{"all interfaces with key", "0.0.0.0", "k", false},
		{"ipv6 any no key", "::", "", true},
		{"specific ip no key", "10.0.0.5", "", true},
		{"specific ip with key", "10.0.0.5", "secret", false},
		{"hostname no key", "example.com", "", true},

		// A key that is only whitespace does not count.
		{"blank key on public host", "0.0.0.0", "   ", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateBindSecurity(&Config{Host: c.host, APIKey: c.apiKey})
			if c.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "api-key")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
