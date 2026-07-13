package main

import (
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestKiroUserAgent(t *testing.T) {
	t.Run("with machine id", func(t *testing.T) {
		ua := kiroUserAgent("deadbeef")
		if !strings.Contains(ua, "aws-sdk-js/"+awsSDKVersion) {
			t.Errorf("ua missing aws-sdk-js version: %q", ua)
		}
		if !strings.Contains(ua, "KiroIDE-"+kiroIDEVersion+"-deadbeef") {
			t.Errorf("ua missing KiroIDE suffix with fingerprint: %q", ua)
		}
		if !strings.Contains(ua, "api/codewhispererstreaming#") {
			t.Errorf("ua missing streaming api marker: %q", ua)
		}
		if !strings.Contains(ua, "lang/js") {
			t.Errorf("ua missing lang/js marker: %q", ua)
		}
	})
	t.Run("without machine id", func(t *testing.T) {
		ua := kiroUserAgent("")
		if !strings.HasSuffix(ua, "KiroIDE-"+kiroIDEVersion) {
			t.Errorf("ua should end with bare KiroIDE version when no fingerprint: %q", ua)
		}
	})
	t.Run("never leaks kiro-anthropic", func(t *testing.T) {
		ua := kiroUserAgent("x")
		if strings.Contains(ua, "kiro-anthropic") {
			t.Errorf("ua must not leak the kiro-anthropic marker: %q", ua)
		}
	})
}

func TestKiroAmzUserAgent(t *testing.T) {
	got := kiroAmzUserAgent("abc")
	if !strings.HasPrefix(got, "aws-sdk-js/"+awsSDKVersion+" KiroIDE-") {
		t.Errorf("unexpected x-amz-user-agent: %q", got)
	}
	if !strings.Contains(got, "abc") {
		t.Errorf("x-amz-user-agent missing fingerprint: %q", got)
	}
}

func TestMachineIDFor(t *testing.T) {
	t.Run("stable for same account", func(t *testing.T) {
		a := machineIDFor("acct-1")
		b := machineIDFor("acct-1")
		if a == "" {
			t.Fatal("machineID should be non-empty for a non-empty account id")
		}
		if a != b {
			t.Errorf("machineID not stable: %q vs %q", a, b)
		}
	})
	t.Run("distinct across accounts", func(t *testing.T) {
		if machineIDFor("acct-1") == machineIDFor("acct-2") {
			t.Error("machineID should differ across accounts")
		}
	})
	t.Run("empty input yields empty output", func(t *testing.T) {
		if machineIDFor("") != "" {
			t.Errorf("machineIDFor(\"\") should be empty, got %q", machineIDFor(""))
		}
	})
}

func TestApplyKiroHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://runtime.us-east-1.kiro.dev/", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyKiroHeaders(req, "tok-123", "fp-abc")

	checks := map[string]string{
		"User-Agent":             "aws-sdk-js/",
		"X-Amz-User-Agent":       "KiroIDE-",
		"X-Amzn-Kiro-Agent-Mode": "vibe",
		"Amz-Sdk-Invocation-Id":  "",
		"Amz-Sdk-Request":        "attempt=1; max=3",
		"Authorization":          "Bearer tok-123",
	}
	for h, wantSub := range checks {
		got := req.Header.Get(h)
		if got == "" {
			t.Errorf("header %q not set", h)
			continue
		}
		if wantSub != "" && !strings.Contains(got, wantSub) {
			t.Errorf("header %q = %q, want substring %q", h, got, wantSub)
		}
	}
	// invocation id must be a uuid
	if matched, _ := regexp.MatchString(`^[0-9a-f-]{36}$`, req.Header.Get("Amz-Sdk-Invocation-Id")); !matched {
		t.Errorf("Amz-Sdk-Invocation-Id not a uuid: %q", req.Header.Get("Amz-Sdk-Invocation-Id"))
	}
}

func TestApplyKiroHeadersEmptyTokenOmitsAuth(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://oidc.us-east-1.amazonaws.com/client/register", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyKiroHeaders(req, "", "")
	if auth := req.Header.Get("Authorization"); auth != "" {
		t.Errorf("empty token must not set Authorization, got %q", auth)
	}
	if ua := req.Header.Get("User-Agent"); !strings.Contains(ua, "KiroIDE-") {
		t.Errorf("UA still impersonates without token: %q", ua)
	}
}

func TestKiroOSPlatform(t *testing.T) {
	// kiroOSPlatform must mirror Node's process.platform exactly, since the
	// AWS SDK for JS embeds that value in the UA and a mismatch makes the
	// request *more* distinguishable, not less.
	want := map[string]string{
		"windows": "win32",
		"darwin":  "darwin", // NOT "macos" — process.platform returns "darwin"
		"linux":   "linux",
	}
	got := kiroOSPlatform()
	if got != want[runtime.GOOS] {
		t.Errorf("kiroOSPlatform() = %q for GOOS=%q, want %q", got, runtime.GOOS, want[runtime.GOOS])
	}
}
