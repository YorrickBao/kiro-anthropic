package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Remote image handling. Anthropic image blocks may reference an image by a
// remote URL (source.type == "url"), but Kiro's runtime only accepts inline
// base64 bytes. This file downloads such URLs and rewrites the block into an
// inline base64 source BEFORE translation, so the translation layer only ever
// sees base64 images and needs no knowledge of remote URLs.

const (
	defaultImageMaxBytes = 10 << 20 // 10 MiB, matching Kiro's practical image cap.
	defaultImageTimeout  = 15 * time.Second
)

// imageFetcher downloads remote (http/https) images referenced by Anthropic
// image blocks and inlines them as base64.
type imageFetcher struct {
	client   *http.Client
	maxBytes int64
	timeout  time.Duration
	// allowPrivate disables the SSRF host guard. It exists only so tests can
	// target loopback httptest servers; production code never sets it.
	allowPrivate bool
}

// newImageFetcher returns a fetcher with production defaults, reusing the
// proxy-aware outbound HTTP client so remote images are pulled through the same
// egress path as every other outbound request.
func newImageFetcher(client *http.Client) *imageFetcher {
	return &imageFetcher{client: client, maxBytes: defaultImageMaxBytes, timeout: defaultImageTimeout}
}

// transformBlocks parses raw content (a string or []block), applies fn to every
// block, and returns the re-serialized content plus whether any block changed.
// Unchanged content is returned byte-for-byte (no re-marshal). It is the shared
// parse→transform→marshal skeleton for message content at any nesting level.
func transformBlocks(raw json.RawMessage, fn func(b *anthropicContentBlock) bool) (json.RawMessage, bool) {
	blocks, err := parseContentBlocks(raw)
	if err != nil || len(blocks) == 0 {
		return raw, false
	}
	changed := false
	for i := range blocks {
		if fn(&blocks[i]) {
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	out, merr := json.Marshal(blocks)
	if merr != nil {
		return raw, false
	}
	return out, true
}

// mapContentBlocks applies fn to every block of every message. A message is
// re-serialized only when at least one of its blocks changed, so every other
// message is left byte-for-byte unchanged.
func mapContentBlocks(areq *anthropicRequest, fn func(msgIdx int, b *anthropicContentBlock) bool) {
	for i := range areq.Messages {
		if out, changed := transformBlocks(areq.Messages[i].Content, func(b *anthropicContentBlock) bool {
			return fn(i, b)
		}); changed {
			areq.Messages[i].Content = out
		}
	}
}

// currentTurnStart returns the index where the current turn begins: the trailing
// run of non-assistant messages ending the request. Messages from here on keep
// their images (the current turn the model must see); anything earlier is
// history. Role "assistant" marks a turn boundary; any other role is treated as
// user, matching convertMessage. Returns len(messages) when there is no
// non-assistant message, so nothing is protected (assistant turns carry no
// images anyway).
func currentTurnStart(messages []anthropicMessage) int {
	last := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" {
			last = i
			break
		}
	}
	if last == len(messages) {
		return last
	}
	start := last
	for i := last - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" {
			start = i
		} else {
			break
		}
	}
	return start
}

// mediaOmittedText is the placeholder used when a history image/document is
// dropped, so the model knows content was present without paying its base64 cost.
// It is newline-wrapped to match convertMessage's own "[unsupported image
// omitted]" note and to keep it from fusing with adjacent text when blocks are
// concatenated.
func mediaOmittedText(typ string) string {
	if typ == "document" {
		return "\n[document omitted]\n"
	}
	return "\n[image omitted]\n"
}

// trimToolResultImages drops image/document blocks nested inside a tool_result's
// content (e.g. screenshots or PDFs returned by a tool), replacing each with a
// placeholder. It returns whether any change was made. A tool_result whose
// content is a plain string is left untouched.
func trimToolResultImages(b *anthropicContentBlock) bool {
	out, changed := transformBlocks(b.Content, func(blk *anthropicContentBlock) bool {
		if blk.Type == "image" || blk.Type == "document" {
			cc := blk.CacheControl // preserve a caching breakpoint on the dropped block
			*blk = anthropicContentBlock{Type: "text", Text: mediaOmittedText(blk.Type), CacheControl: cc}
			return true
		}
		return false
	})
	if changed {
		b.Content = out
	}
	return changed
}

// processImages prepares an Anthropic request's images for Kiro in a single pass
// over the messages:
//   - In the current turn, remote (http/https) image URLs are inlined as base64,
//     because Kiro only accepts inline image bytes. A block that cannot be
//     fetched is left as a url source and downstream-skipped.
//   - In history, every image and document is dropped to a placeholder —
//     including media nested in tool_result blocks — since stale history base64
//     would inflate the raw-byte token estimate returned to callers.
//
// The current turn (the trailing run of non-assistant messages) keeps all its
// images, so the model sees what the user just sent.
func processImages(ctx context.Context, areq *anthropicRequest, fetcher *imageFetcher) {
	turnStart := currentTurnStart(areq.Messages)
	mapContentBlocks(areq, func(i int, b *anthropicContentBlock) bool {
		if i < turnStart {
			// History: drop images/documents, including those nested in tool_result.
			if b.Type == "image" || b.Type == "document" {
				cc := b.CacheControl // preserve a caching breakpoint on the dropped block
				*b = anthropicContentBlock{Type: "text", Text: mediaOmittedText(b.Type), CacheControl: cc}
				return true
			}
			if b.Type == "tool_result" {
				return trimToolResultImages(b)
			}
			return false
		}
		// Current turn: inline remote url images only.
		if b.Type == "image" && b.Source != nil &&
			strings.EqualFold(b.Source.Type, "url") && b.Source.URL != "" {
			mediaType, data, ferr := fetcher.fetch(ctx, b.Source.URL)
			if ferr != nil {
				if os.Getenv("KIRO_DEBUG") != "" {
					fmt.Fprintf(os.Stderr, "[kiro-debug] image url fetch failed (%s): %v\n", b.Source.URL, ferr)
				}
				return false // leave the url source; convertImage will skip it.
			}
			b.Source = &anthropicImageSource{Type: "base64", MediaType: mediaType, Data: data}
			return true
		}
		return false
	})
}

// fetch downloads a single remote image and returns its media type (e.g.
// "image/png") and base64-encoded bytes. It enforces the http/https scheme, an
// SSRF guard against private/loopback destinations, a size cap, and a
// Kiro-supported content-type.
func (f *imageFetcher) fetch(ctx context.Context, rawURL string) (mediaType, dataB64 string, err error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", "", fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", fmt.Errorf("unsupported url scheme %q", u.Scheme)
	}
	if !f.allowPrivate {
		if gerr := guardImageHost(ctx, u.Hostname()); gerr != nil {
			return "", "", gerr
		}
	}

	cctx := ctx
	if f.timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, f.timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "kiro-anthropic/"+version)
	req.Header.Set("Accept", "image/png,image/jpeg,image/gif,image/webp,image/*")
	resp, err := f.doWithGuardedRedirects().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("download image: HTTP %d", resp.StatusCode)
	}

	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	if _, ok := mediaTypeToKiroImageFormat(ct); !ok {
		return "", "", fmt.Errorf("unsupported image content-type %q", ct)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return "", "", err
	}
	if int64(len(data)) > f.maxBytes {
		return "", "", fmt.Errorf("image exceeds %d byte limit", f.maxBytes)
	}
	return ct, base64.StdEncoding.EncodeToString(data), nil
}

// doWithGuardedRedirects returns a shallow copy of the outbound client whose
// only change is a CheckRedirect that re-applies the scheme and SSRF host guard
// on every hop. Without this, a public URL that passes the initial guard could
// 302-redirect to a private/loopback/metadata address and be followed blindly,
// defeating the guard entirely. The copy shares the underlying Transport
// (connection pool, proxy settings), so nothing else changes.
func (f *imageFetcher) doWithGuardedRedirects() *http.Client {
	c := *f.client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("unsupported redirect scheme %q", req.URL.Scheme)
		}
		if !f.allowPrivate {
			return guardImageHost(req.Context(), req.URL.Hostname())
		}
		return nil
	}
	return &c
}

// guardImageHost rejects hosts that resolve to loopback, private, link-local,
// or otherwise non-public addresses, mitigating SSRF via attacker-supplied
// image URLs. It resolves the hostname and checks every returned address, and
// is re-applied on every redirect hop (see doWithGuardedRedirects).
//
// The check is best-effort with two known gaps: it does not defend against DNS
// rebinding between this lookup and the actual dial, and when an outbound proxy
// is configured the proxy performs its own resolution/connection, so this
// host-level guard does not see the address the proxy ultimately reaches. Under
// a proxy, egress safety therefore depends on the proxy's own policy.
func guardImageHost(ctx context.Context, host string) error {
	if host == "" {
		return fmt.Errorf("empty image host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if isDisallowedIP(ip) {
			return fmt.Errorf("blocked image host %q", host)
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("resolve image host %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("image host %q has no addresses", host)
	}
	for _, a := range addrs {
		if isDisallowedIP(a.IP) {
			return fmt.Errorf("blocked image host %q -> %s", host, a.IP)
		}
	}
	return nil
}

// isDisallowedIP reports whether an IP must not be used as an image source:
// loopback, private (RFC1918 / unique-local fc00::/7), link-local, multicast,
// or unspecified addresses are all rejected.
func isDisallowedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified()
}
