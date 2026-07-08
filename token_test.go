package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTokenExpiry(t *testing.T) {
	// Assert the exact parsed instant (not merely "parses to non-zero"), so a
	// wrong-layout parse producing the wrong time would be caught.
	cases := []struct {
		in   string
		want time.Time
	}{
		{"2026-07-06T09:15:09.273Z", time.Date(2026, 7, 6, 9, 15, 9, 273_000_000, time.UTC)},
		{"2026-07-06T09:15:09Z", time.Date(2026, 7, 6, 9, 15, 9, 0, time.UTC)},
		{"2026-07-06T09:15:09.273000000Z", time.Date(2026, 7, 6, 9, 15, 9, 273_000_000, time.UTC)},
	}
	for _, c := range cases {
		got := (Token{ExpiresAt: c.in}).expiry()
		assert.Truef(t, got.Equal(c.want), "expiry(%q) = %v, want %v", c.in, got, c.want)
	}
	assert.True(t, (Token{ExpiresAt: ""}).expiry().IsZero(), "empty expiresAt should be zero")
	assert.True(t, (Token{ExpiresAt: "not-a-date"}).expiry().IsZero(), "garbage expiresAt should be zero")
}

func TestRegionResolution(t *testing.T) {
	// region(): --region override > token region > us-east-1 default.
	assert.Equal(t, "eu-west-1",
		(&TokenStore{cfg: &Config{Region: "eu-west-1"}, tok: Token{Region: "ap-south-1"}}).region(),
		"--region should win over token region")
	assert.Equal(t, "ap-south-1",
		(&TokenStore{cfg: &Config{}, tok: Token{Region: "ap-south-1"}}).region(),
		"token region should be used when no override")
	assert.Equal(t, "us-east-1",
		(&TokenStore{cfg: &Config{}, tok: Token{}}).region(),
		"should default to us-east-1")
}

func TestApiRegionResolution(t *testing.T) {
	// apiRegion(): --api-region override > region() (which itself falls back).
	assert.Equal(t, "eu-central-1",
		(&TokenStore{cfg: &Config{APIRegion: "eu-central-1"}, tok: Token{Region: "us-east-1"}}).apiRegion(),
		"--api-region should override the SSO region for API endpoints")

	// Without --api-region, apiRegion tracks region() exactly.
	s := &TokenStore{cfg: &Config{Region: "us-east-1"}, tok: Token{Region: "ap-south-1"}}
	assert.Equal(t, s.region(), s.apiRegion(),
		"api region should default to the SSO region when not overridden")

	// --api-region does not affect the SSO region used for OIDC refresh.
	split := &TokenStore{cfg: &Config{Region: "us-east-1", APIRegion: "eu-central-1"}}
	assert.Equal(t, "us-east-1", split.region(), "SSO region stays put")
	assert.Equal(t, "eu-central-1", split.apiRegion(), "API region diverges")
}

func TestTokenValid(t *testing.T) {
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	assert.True(t, (Token{ExpiresAt: "2030-01-01T00:00:00Z", AccessToken: "x"}).valid(now),
		"future expiry should be valid")
	assert.False(t, (Token{ExpiresAt: "2020-01-01T00:00:00Z", AccessToken: "x"}).valid(now),
		"past expiry should be invalid")
	// Unparseable expiry: usable iff there is an access token.
	assert.True(t, (Token{ExpiresAt: "???", AccessToken: "x"}).valid(now),
		"unparseable expiry with token should be valid")
	assert.False(t, (Token{ExpiresAt: "???"}).valid(now),
		"unparseable expiry without token should be invalid")
}
