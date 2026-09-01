package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// kiro-cli-style request headers.
//
// The official kiro-cli (Rust) is the reference client for the Kiro backend;
// every request it sends carries a plain "kiro-cli/<version>" User-Agent plus
// the AWS SDK internal headers (x-amz-user-agent, amz-sdk-invocation-id,
// amz-sdk-request). Sending our own "kiro-anthropic/<version>" UA instead
// makes the traffic trivially distinguishable from a real client in
// server-side logs, which is an avoidable risk on a backend that is known to
// key off the user-agent header (see the AWS blog on Q Developer user-agent
// markers and CloudTrail). The helpers below reproduce the official CLI's
// wire signature so our requests blend in.
//
// kiroCLIVersion tracks the kiro-cli release train; bump it when the real CLI
// moves far enough ahead that an old version stands out. Alignment history and
// the re-alignment checklist live in README "与 kiro-cli 版本对齐".
// ---------------------------------------------------------------------------

const kiroCLIVersion = "2.20.1"

// kiroUABase renders the official kiro-cli User-Agent template:
//
//	KiroCLI/<version> KAS/ md/appVersion-<version> app/AmazonQ-For-CLI
//
// (extracted from the kiro-cli 2.20.1 binary; the KAS segment ships empty).
// machineID, when non-empty, is appended as a per-installation fingerprint so
// an account pool does not look like N sessions on a single anonymous host.
func kiroUABase(machineID string) string {
	ua := "KiroCLI/" + kiroCLIVersion
	if machineID != "" {
		ua += "-" + machineID
	}
	return ua + " KAS/ md/appVersion-" + kiroCLIVersion + " app/AmazonQ-For-CLI"
}

// kiroUserAgent returns the User-Agent the official kiro-cli sends.
func kiroUserAgent(machineID string) string {
	return kiroUABase(machineID)
}

// kiroAmzUserAgent returns the x-amz-user-agent value the official kiro-cli
// sends: the bare CLI marker without the per-installation fingerprint.
func kiroAmzUserAgent(string) string {
	return "KiroCLI/" + kiroCLIVersion
}

// applyKiroHeaders stamps a request with the full set of headers the official
// kiro-cli sends to the AWS/Kiro backend. Content-Type, X-Amz-Target and the
// like are left to the caller since they vary per operation; this only sets
// the identification + auth headers that are common to every backend call.
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
