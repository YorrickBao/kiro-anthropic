package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"runtime"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Kiro IDE-style request headers.
//
// The Kiro IDE is an Electron app that talks to AWS CodeWhisperer / Amazon Q
// through the AWS SDK for JavaScript. Every request it sends carries a
// characteristic User-Agent (aws-sdk-js/...) plus a set of AWS SDK internal
// headers (x-amz-user-agent, amz-sdk-invocation-id, amz-sdk-request). Sending
// our own "kiro-anthropic/<version>" UA instead makes the traffic trivially
// distinguishable from a real IDE in server-side logs, which is an avoidable
// risk on a backend that is known to key off the user-agent header (see the
// AWS blog on Q Developer user-agent markers and CloudTrail). The helpers below
// reproduce the IDE's wire signature so our requests blend in.
//
// The UA strings mirror what the official Kiro IDE emits; the version
// constants are bumped manually to track the IDE release train.
// ---------------------------------------------------------------------------

// kiroIDEVersion is the Kiro IDE release this impersonates. Bump it when the
// real IDE moves far enough ahead that an old version stands out.
const kiroIDEVersion = "0.12.155"

// awsSDKVersion mirrors the aws-sdk-js version embedded in the IDE's UA.
const awsSDKVersion = "1.0.34"

// nodeVersion mirrored in the IDE's UA (Kiro is an Electron app).
const nodeVersion = "22.22.0"

// kiroUserAgent returns the User-Agent a real Kiro IDE sends. machineID, when
// non-empty, is appended as a per-installation fingerprint the way the IDE
// appends its device id, so an account pool does not look like N sessions on a
// single anonymous host.
func kiroUserAgent(machineID string) string {
	suffix := "KiroIDE-" + kiroIDEVersion
	if machineID != "" {
		suffix += "-" + machineID
	}
	return "aws-sdk-js/" + awsSDKVersion + " ua/2.1 os/" + kiroOSPlatform() + "#" + kiroOSRelease() +
		" lang/js md/nodejs#" + nodeVersion + " api/codewhispererstreaming#" + awsSDKVersion +
		" m/E " + suffix
}

// kiroAmzUserAgent returns the x-amz-user-agent value a real Kiro IDE sends.
func kiroAmzUserAgent(machineID string) string {
	suffix := "KiroIDE-" + kiroIDEVersion
	if machineID != "" {
		suffix += "-" + machineID
	}
	return "aws-sdk-js/" + awsSDKVersion + " " + suffix
}

// applyKiroHeaders stamps a request with the full set of headers a Kiro IDE
// sends to the AWS/Kiro backend. Content-Type, X-Amz-Target and the like are
// left to the caller since they vary per operation; this only sets the
// identification + auth headers that are common to every backend call.
//
// token may be empty for unauthenticated calls (none currently, but kept for
// symmetry); machineID should come from machineIDFor(accountID).
func applyKiroHeaders(req *http.Request, token, machineID string) {
	req.Header.Set("User-Agent", kiroUserAgent(machineID))
	req.Header.Set("X-Amz-User-Agent", kiroAmzUserAgent(machineID))
	req.Header.Set("X-Amzn-Kiro-Agent-Mode", "vibe")
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.NewString())
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// machineIDFor returns a stable per-account machine fingerprint. Real Kiro
// installs each report their own device id; giving every pooled account a
// distinct, deterministic id avoids the "one host, many accounts" pattern
// while keeping the same id across restarts for the same account.
func machineIDFor(accountID string) string {
	if accountID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("kiro-device:" + accountID))
	return hex.EncodeToString(sum[:])
}

// kiroOSPlatform maps Go's GOOS to the platform token the JS SDK emits.
// The AWS SDK for JavaScript derives this from Node's process.platform, which
// returns "darwin" on macOS (not "macos"), "win32" on Windows, "linux" on
// Linux. Matching these exactly is what makes the UA indistinguishable from a
// real Kiro IDE.
func kiroOSPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return "linux"
	}
}

// kiroOSRelease returns an OS version string for the UA. The real IDE reports
// os.release() from node; we don't have a portable equivalent that is worth
// the platform-specific code, so a plausible fixed value per OS is used. The
// exact version is not load-bearing for blending in.
func kiroOSRelease() string {
	switch runtime.GOOS {
	case "windows":
		return "10.0.19045"
	case "darwin":
		return "24.6.0"
	default:
		return "6.8.0"
	}
}
