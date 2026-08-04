//go:build liverefreshtest

// Live probe: fire two refresh_token grants CONCURRENTLY against AWS SSO-OIDC
// using the SAME refresh token (bypassing singleflight) to learn whether AWS
// rotates with a grace period (both succeed, tokens fork) or strictly
// single-use (one fails with invalid_grant).
//
// This consumes one refresh-token rotation and mutates your local token chain.
// Run explicitly:
//
//	go test -tags liverefreshtest -run TestLiveRefreshRace -v ./.
package main

import (
	"context"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

func TestLiveRefreshRace(t *testing.T) {
	tokenFile := os.Getenv("TOKEN_FILE")
	if tokenFile == "" {
		tokenFile = liveDefaultTokenFile()
	}
	t.Logf("using token file: %s", tokenFile)

	tok, err := loadToken(tokenFile)
	if err != nil {
		t.Fatalf("loadToken: %v", err)
	}
	if tok.RefreshToken == "" {
		t.Fatalf("token file has no refreshToken")
	}
	clientID, clientSecret := findClientRegistration(tokenFile, tok.ClientIDHash)
	if clientID == "" || clientSecret == "" {
		t.Fatalf("could not locate client registration for %q", tok.ClientIDHash)
	}
	region := tok.Region
	if region == "" {
		region = "us-east-1"
	}
	t.Logf("region=%s clientID=%s... profileArn set=%v", region, clientID[:8], tok.ProfileArn != "")

	// Same starting refresh token for both goroutines — this is the race we
	// want to observe. singleflight is deliberately NOT used here.
	base := StoredAccount{
		Region:       region,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RefreshToken: tok.RefreshToken,
	}
	client := newHTTPClient(defaultProxyURL)

	type result struct {
		access, refresh, expiresAt string
		err                        error
		dur                        time.Duration
	}
	var wg sync.WaitGroup
	res := make([]result, 2)
	start := time.Now()
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := time.Now()
			a, r, exp, err := refreshAccountToken(context.Background(), client, base)
			res[i] = result{access: a, refresh: r, expiresAt: exp, err: err, dur: time.Since(s)}
		}(i)
	}
	wg.Wait()
	total := time.Since(start)

	for i, r := range res {
		if r.err != nil {
			t.Logf("call[%d]: FAILED after %v: %v", i, r.dur, r.err)
		} else {
			t.Logf("call[%d]: ok after %v  access=%s... refresh=%s... expires=%s",
				i, r.dur, short(r.access), short(r.refresh), r.expiresAt)
		}
	}
	t.Logf("total elapsed: %v", total)

	// Classification.
	switch {
	case res[0].err != nil && res[1].err != nil:
		t.Logf("\nRESULT: both failed — likely a network/credential problem, not a rotation answer.")

	case res[0].err == nil && res[1].err == nil:
		t.Logf("\nRESULT: BOTH SUCCEEDED with the same starting refresh token.")
		if res[0].refresh == res[1].refresh {
			t.Logf("  refresh tokens are IDENTICAL → AWS returned the same rotated token to both (no fork).")
		} else {
			t.Logf("  refresh tokens DIFFER → the token chain FORKED into two valid refresh tokens (grace window exists).")
			t.Logf("  Verifying both are actually usable by refreshing each independently...")
			verifyFork(t, client, region, clientID, clientSecret, res[0].refresh, "forkA")
			verifyFork(t, client, region, clientID, clientSecret, res[1].refresh, "forkB")
		}

	default:
		ok, fail := 0, 1
		if res[0].err != nil {
			ok, fail = 1, 0
		}
		t.Logf("\nRESULT: ONE succeeded, ONE failed (invalid_grant expected).")
		t.Logf("  → NO grace period: refresh tokens are strictly single-use. The second")
		t.Logf("    caller's request landed after the first rotated the token, so it was rejected.")
		_ = ok
		_ = fail
	}
}

func verifyFork(t *testing.T, client *http.Client, region, cid, secret, refresh, label string) {
	t.Helper()
	a := StoredAccount{Region: region, ClientID: cid, ClientSecret: secret, RefreshToken: refresh}
	if _, r2, _, err := refreshAccountToken(context.Background(), client, a); err != nil {
		t.Logf("    %s: NOT usable on its own follow-up refresh: %v", label, err)
	} else {
		t.Logf("    %s: usable on follow-up refresh (new refresh=%s...)", label, short(r2))
	}
}

func short(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

func liveDefaultTokenFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home + "/.aws/sso/cache/kiro-auth-token.json"
}
