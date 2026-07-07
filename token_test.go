package main

import (
	"testing"
	"time"
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
		if !got.Equal(c.want) {
			t.Errorf("expiry(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if !(Token{ExpiresAt: ""}).expiry().IsZero() {
		t.Errorf("empty expiresAt should be zero")
	}
	if !(Token{ExpiresAt: "not-a-date"}).expiry().IsZero() {
		t.Errorf("garbage expiresAt should be zero")
	}
}

func TestTokenValid(t *testing.T) {
	now := time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC)

	if !(Token{ExpiresAt: "2030-01-01T00:00:00Z", AccessToken: "x"}).valid(now) {
		t.Errorf("future expiry should be valid")
	}
	if (Token{ExpiresAt: "2020-01-01T00:00:00Z", AccessToken: "x"}).valid(now) {
		t.Errorf("past expiry should be invalid")
	}
	// Unparseable expiry: usable iff there is an access token.
	if !(Token{ExpiresAt: "???", AccessToken: "x"}).valid(now) {
		t.Errorf("unparseable expiry with token should be valid")
	}
	if (Token{ExpiresAt: "???"}).valid(now) {
		t.Errorf("unparseable expiry without token should be invalid")
	}
}
