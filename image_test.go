package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// onePNGPixel is a valid 1x1 PNG; the exact bytes do not matter for these tests,
// only that they round-trip through base64.
var onePNGPixel = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89,
}

func testFetcher(srv *httptest.Server) *imageFetcher {
	f := newImageFetcher(srv.Client())
	f.allowPrivate = true // httptest binds to loopback; bypass the SSRF guard.
	return f
}

func TestImageFetcherFetchSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(onePNGPixel)
	}))
	defer srv.Close()

	mediaType, data, err := testFetcher(srv).fetch(context.Background(), srv.URL+"/pixel.png")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if mediaType != "image/png" {
		t.Errorf("mediaType = %q, want image/png", mediaType)
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	if string(decoded) != string(onePNGPixel) {
		t.Errorf("decoded bytes mismatch: got %d bytes", len(decoded))
	}
}

func TestImageFetcherStripsContentTypeParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg; charset=binary")
		_, _ = w.Write([]byte("jpegbytes"))
	}))
	defer srv.Close()

	mediaType, _, err := testFetcher(srv).fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if mediaType != "image/jpeg" {
		t.Errorf("mediaType = %q, want image/jpeg", mediaType)
	}
}

func TestImageFetcherRejectsContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>not an image</html>"))
	}))
	defer srv.Close()

	if _, _, err := testFetcher(srv).fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error for non-image content-type, got nil")
	}
}

func TestImageFetcherSizeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(make([]byte, 4096))
	}))
	defer srv.Close()

	f := testFetcher(srv)
	f.maxBytes = 1024
	if _, _, err := f.fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("expected size-limit error, got nil")
	}
}

func TestImageFetcherRejectsHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	if _, _, err := testFetcher(srv).fetch(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error for 404, got nil")
	}
}

func TestImageFetcherRejectsScheme(t *testing.T) {
	f := newImageFetcher(http.DefaultClient)
	if _, _, err := f.fetch(context.Background(), "ftp://example.com/a.png"); err == nil {
		t.Fatal("expected error for non-http scheme, got nil")
	}
	if _, _, err := f.fetch(context.Background(), "data:image/png;base64,AAAA"); err == nil {
		t.Fatal("expected error for data scheme, got nil")
	}
}

func TestImageFetcherSSRFGuardBlocksLoopback(t *testing.T) {
	// allowPrivate defaults to false, so a loopback target must be refused
	// before any network call.
	f := newImageFetcher(http.DefaultClient)
	if _, _, err := f.fetch(context.Background(), "http://127.0.0.1:9/secret.png"); err == nil {
		t.Fatal("expected SSRF guard to block loopback, got nil")
	}
}

func TestGuardImageHostBlocksLocalhost(t *testing.T) {
	err := guardImageHost(context.Background(), "localhost")
	if err == nil || !strings.Contains(err.Error(), "blocked image host") {
		t.Fatalf("guardImageHost(localhost) = %v, want resolved loopback rejection", err)
	}
}

func TestGuardedRedirectPolicy(t *testing.T) {
	f := newImageFetcher(http.DefaultClient) // allowPrivate == false
	check := f.doWithGuardedRedirects().CheckRedirect
	if check == nil {
		t.Fatal("CheckRedirect not set")
	}
	mkReq := func(rawURL string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatalf("new request %q: %v", rawURL, err)
		}
		return req
	}

	// Redirect to loopback / metadata / private must be refused (the SSRF
	// bypass this guard exists to close).
	for _, u := range []string{
		"http://127.0.0.1/x.png",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/x.png",
		"ftp://example.com/x.png", // non-http scheme on redirect
	} {
		if err := check(mkReq(u), nil); err == nil {
			t.Errorf("redirect to %q should be blocked, got nil", u)
		}
	}

	// Redirect to a public IP literal is allowed (no DNS needed).
	if err := check(mkReq("http://8.8.8.8/x.png"), nil); err != nil {
		t.Errorf("redirect to public IP should be allowed, got %v", err)
	}

	// Too many hops is refused regardless of target.
	via := make([]*http.Request, 10)
	if err := check(mkReq("http://8.8.8.8/x.png"), via); err == nil {
		t.Error("expected too-many-redirects error, got nil")
	}
}

func TestGuardedRedirectPolicyAllowPrivate(t *testing.T) {
	f := newImageFetcher(http.DefaultClient)
	f.allowPrivate = true
	check := f.doWithGuardedRedirects().CheckRedirect
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/x.png", nil)
	if err := check(req, nil); err != nil {
		t.Errorf("allowPrivate should permit loopback redirect, got %v", err)
	}
}

func TestIsDisallowedIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"10.1.2.3", true},
		{"192.168.0.1", true},
		{"172.16.5.4", true},
		{"169.254.1.1", true}, // link-local
		{"0.0.0.0", true},     // unspecified
		{"224.0.0.1", true},   // multicast
		{"fc00::1", true},     // unique-local
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2606:4700:4700::1111", false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test ip %q", c.ip)
		}
		if got := isDisallowedIP(ip); got != c.want {
			t.Errorf("isDisallowedIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

func mustBlocks(t *testing.T, blocks []anthropicContentBlock) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal blocks: %v", err)
	}
	return raw
}

// contentImageCount parses a message content and counts image blocks.
func contentImageCount(t *testing.T, raw json.RawMessage) int {
	t.Helper()
	blocks, err := parseContentBlocks(raw)
	if err != nil {
		t.Fatalf("parse content: %v", err)
	}
	n := 0
	for _, b := range blocks {
		if b.Type == "image" {
			n++
		}
	}
	return n
}

func TestProcessImagesInlinesCurrentTurnURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(onePNGPixel)
	}))
	defer srv.Close()

	areq := &anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{
			{Type: "text", Text: "look:"},
			{Type: "image", Source: &anthropicImageSource{Type: "url", URL: srv.URL + "/x.png"}},
		})},
	}}
	processImages(context.Background(), areq, testFetcher(srv))

	blocks, err := parseContentBlocks(areq.Messages[0].Content)
	if err != nil {
		t.Fatalf("parse rewritten content: %v", err)
	}
	var img *anthropicContentBlock
	for i := range blocks {
		if blocks[i].Type == "image" {
			img = &blocks[i]
		}
	}
	if img == nil {
		t.Fatal("current-turn url image must be inlined, not dropped")
	}
	if img.Source.Type != "base64" || img.Source.MediaType != "image/png" {
		t.Errorf("source = %+v, want base64 image/png", img.Source)
	}
	if _, ok := convertImage(*img); !ok {
		t.Error("inlined block must convert to a Kiro image")
	}
}

func TestProcessImagesLeavesFailedURLUntouched(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	areq := &anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{
			{Type: "image", Source: &anthropicImageSource{Type: "url", URL: srv.URL + "/missing.png"}},
		})},
	}}
	processImages(context.Background(), areq, testFetcher(srv))

	blocks, _ := parseContentBlocks(areq.Messages[0].Content)
	if len(blocks) != 1 || blocks[0].Source == nil || !strings.EqualFold(blocks[0].Source.Type, "url") {
		t.Fatalf("failed download should leave url source intact, got %+v", blocks)
	}
	if _, ok := convertImage(blocks[0]); ok {
		t.Error("convertImage should skip an unresolved url source")
	}
}

func TestProcessImagesStringContentUntouched(t *testing.T) {
	areq := &anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: json.RawMessage(`"just text"`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"reply"}]`)},
		{Role: "user", Content: json.RawMessage(`"plain"`)},
	}}
	processImages(context.Background(), areq, newImageFetcher(http.DefaultClient))
	if string(areq.Messages[0].Content) != `"just text"` {
		t.Errorf("string content changed: %s", areq.Messages[0].Content)
	}
	if string(areq.Messages[1].Content) != `[{"type":"text","text":"reply"}]` {
		t.Errorf("assistant content changed: %s", areq.Messages[1].Content)
	}
}

func TestProcessImagesDropsHistoryKeepsCurrent(t *testing.T) {
	histImg := anthropicContentBlock{Type: "image", Source: &anthropicImageSource{Type: "base64", MediaType: "image/png", Data: "HISTORY-BYTES"}}
	curImg := anthropicContentBlock{Type: "image", Source: &anthropicImageSource{Type: "base64", MediaType: "image/png", Data: "CURRENT-BYTES"}}
	areq := &anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "first"}, histImg})},
		{Role: "assistant", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "ok"}})},
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "now"}, curImg})},
	}}
	before := len(areq.Messages[0].Content)

	processImages(context.Background(), areq, newImageFetcher(http.DefaultClient))

	if got := contentImageCount(t, areq.Messages[0].Content); got != 0 {
		t.Errorf("history user: got %d images, want 0", got)
	}
	if got := contentImageCount(t, areq.Messages[2].Content); got != 1 {
		t.Errorf("current user: got %d images, want 1", got)
	}
	if len(areq.Messages[0].Content) >= before {
		t.Errorf("history content did not shrink: %d -> %d", before, len(areq.Messages[0].Content))
	}
}

// #1: consecutive same-role user messages form one turn — all keep their images.
func TestProcessImagesConsecutiveUsersAllKept(t *testing.T) {
	img := func(data string) anthropicContentBlock {
		return anthropicContentBlock{Type: "image", Source: &anthropicImageSource{Type: "base64", MediaType: "image/png", Data: data}}
	}
	areq := &anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{img("A")})},
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{img("B")})},
	}}
	processImages(context.Background(), areq, newImageFetcher(http.DefaultClient))
	if got := contentImageCount(t, areq.Messages[0].Content); got != 1 {
		t.Errorf("first of consecutive users: got %d images, want 1", got)
	}
	if got := contentImageCount(t, areq.Messages[1].Content); got != 1 {
		t.Errorf("second of consecutive users: got %d images, want 1", got)
	}
}

// A trailing assistant prefill still protects the preceding user turn.
func TestProcessImagesPrefillProtectsUserTurn(t *testing.T) {
	img := func(data string) anthropicContentBlock {
		return anthropicContentBlock{Type: "image", Source: &anthropicImageSource{Type: "base64", MediaType: "image/png", Data: data}}
	}
	areq := &anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{img("HIST")})},
		{Role: "assistant", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "ok"}})},
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{img("CUR")})},
		{Role: "assistant", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "prefill"}})},
	}}
	processImages(context.Background(), areq, newImageFetcher(http.DefaultClient))
	if got := contentImageCount(t, areq.Messages[0].Content); got != 0 {
		t.Errorf("history user: got %d images, want 0", got)
	}
	if got := contentImageCount(t, areq.Messages[2].Content); got != 1 {
		t.Errorf("current user before prefill: got %d images, want 1", got)
	}
}

// #3: images nested in a history tool_result are dropped too.
func TestProcessImagesTrimsToolResultImages(t *testing.T) {
	toolResult := anthropicContentBlock{
		Type: "tool_result", ToolUseID: "t1",
		Content: mustBlocks(t, []anthropicContentBlock{
			{Type: "image", Source: &anthropicImageSource{Type: "base64", MediaType: "image/png", Data: "TOOL-IMG"}},
		}),
	}
	areq := &anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{toolResult})},
		{Role: "assistant", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "ok"}})},
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{
			{Type: "image", Source: &anthropicImageSource{Type: "base64", MediaType: "image/png", Data: "CUR"}},
		})},
	}}
	before := len(areq.Messages[0].Content)

	processImages(context.Background(), areq, newImageFetcher(http.DefaultClient))

	tr, _ := parseContentBlocks(areq.Messages[0].Content)
	if len(tr) != 1 || tr[0].Type != "tool_result" {
		t.Fatalf("tool_result block lost: %+v", tr)
	}
	nested, _ := parseContentBlocks(tr[0].Content)
	for _, b := range nested {
		if b.Type == "image" {
			t.Errorf("nested tool_result image was not dropped: %+v", b)
		}
	}
	if len(areq.Messages[0].Content) >= before {
		t.Errorf("tool_result content did not shrink: %d -> %d", before, len(areq.Messages[0].Content))
	}
	if got := contentImageCount(t, areq.Messages[2].Content); got != 1 {
		t.Errorf("current user image not kept: got %d", got)
	}
}

// #2: a non-lowercase role is still treated as the current user turn.
func TestProcessImagesRoleCaseInsensitive(t *testing.T) {
	img := func(data string) anthropicContentBlock {
		return anthropicContentBlock{Type: "image", Source: &anthropicImageSource{Type: "base64", MediaType: "image/png", Data: data}}
	}
	areq := &anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{img("HIST")})},
		{Role: "assistant", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "ok"}})},
		{Role: "User", Content: mustBlocks(t, []anthropicContentBlock{img("CUR")})},
	}}
	processImages(context.Background(), areq, newImageFetcher(http.DefaultClient))
	if got := contentImageCount(t, areq.Messages[0].Content); got != 0 {
		t.Errorf("history user: got %d images, want 0", got)
	}
	if got := contentImageCount(t, areq.Messages[2].Content); got != 1 {
		t.Errorf("current 'User' turn: got %d images, want 1 (kept)", got)
	}
}

// History document blocks (base64 PDFs) are dropped with their own placeholder.
func TestProcessImagesDropsHistoryDocument(t *testing.T) {
	doc := anthropicContentBlock{Type: "document", Source: &anthropicImageSource{Type: "base64", MediaType: "application/pdf", Data: "PDF-BYTES"}}
	areq := &anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "see doc"}, doc})},
		{Role: "assistant", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "ok"}})},
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "now"}})},
	}}
	before := len(areq.Messages[0].Content)

	processImages(context.Background(), areq, newImageFetcher(http.DefaultClient))

	blocks, _ := parseContentBlocks(areq.Messages[0].Content)
	var placeholder string
	for _, b := range blocks {
		if b.Type == "document" {
			t.Errorf("history document was not dropped: %+v", b)
		}
		if b.Type == "text" && b.Text == "\n[document omitted]\n" {
			placeholder = b.Text
		}
	}
	if placeholder != "\n[document omitted]\n" {
		t.Errorf("document placeholder = %q, want newline-wrapped [document omitted]", placeholder)
	}
	if len(areq.Messages[0].Content) >= before {
		t.Errorf("history content did not shrink: %d -> %d", before, len(areq.Messages[0].Content))
	}
}

// A history tool_result whose content is a plain string is left untouched.
func TestProcessImagesStringToolResultUntouched(t *testing.T) {
	areq := &anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{
			{Type: "tool_result", ToolUseID: "t1", Content: json.RawMessage(`"plain result"`)},
		})},
		{Role: "assistant", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "ok"}})},
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "now"}})},
	}}
	orig := string(areq.Messages[0].Content)
	processImages(context.Background(), areq, newImageFetcher(http.DefaultClient))
	if string(areq.Messages[0].Content) != orig {
		t.Errorf("string tool_result changed: %s", areq.Messages[0].Content)
	}
}

// The current turn's tool_result keeps its images (only history is trimmed).
func TestProcessImagesCurrentTurnToolResultKept(t *testing.T) {
	img := anthropicContentBlock{Type: "image", Source: &anthropicImageSource{Type: "base64", MediaType: "image/png", Data: "CUR"}}
	areq := &anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "first"}})},
		{Role: "assistant", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "ok"}})},
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{
			{Type: "tool_result", ToolUseID: "t1", Content: mustBlocks(t, []anthropicContentBlock{img})},
		})},
	}}
	processImages(context.Background(), areq, newImageFetcher(http.DefaultClient))
	tr, _ := parseContentBlocks(areq.Messages[2].Content)
	if len(tr) != 1 || tr[0].Type != "tool_result" {
		t.Fatalf("current tool_result lost: %+v", tr)
	}
	nested, _ := parseContentBlocks(tr[0].Content)
	if len(nested) != 1 || nested[0].Type != "image" {
		t.Errorf("current tool_result image not kept: %+v", nested)
	}
}

// cache_control survives re-serialization: kept on a sibling block, and migrated
// to the placeholder when the carrying image/document is dropped.
func TestProcessImagesPreservesCacheControl(t *testing.T) {
	areq := &anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{
			{Type: "text", Text: "spec", CacheControl: json.RawMessage(`{"type":"ephemeral"}`)},
			{Type: "image", Source: &anthropicImageSource{Type: "base64", MediaType: "image/png", Data: "HIST"}, CacheControl: json.RawMessage(`{"type":"ephemeral"}`)},
		})},
		{Role: "assistant", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "ok"}})},
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "now"}})},
	}}
	processImages(context.Background(), areq, newImageFetcher(http.DefaultClient))
	blocks, _ := parseContentBlocks(areq.Messages[0].Content)
	var spec, placeholder *anthropicContentBlock
	for i := range blocks {
		if blocks[i].Type == "text" && blocks[i].Text == "spec" {
			spec = &blocks[i]
		}
		if blocks[i].Type == "text" && blocks[i].Text == "\n[image omitted]\n" {
			placeholder = &blocks[i]
		}
	}
	if spec == nil {
		t.Fatal("text block lost")
	}
	if string(spec.CacheControl) != `{"type":"ephemeral"}` {
		t.Errorf("sibling cache_control dropped: %q", string(spec.CacheControl))
	}
	if placeholder == nil {
		t.Fatal("placeholder block lost")
	}
	if string(placeholder.CacheControl) != `{"type":"ephemeral"}` {
		t.Errorf("dropped image's cache_control not migrated to placeholder: %q", string(placeholder.CacheControl))
	}
}

// Placeholder text is fixed exactly, distinct for image vs document.
func TestProcessImagesPlaceholderText(t *testing.T) {
	img := anthropicContentBlock{Type: "image", Source: &anthropicImageSource{Type: "base64", MediaType: "image/png", Data: "I"}}
	doc := anthropicContentBlock{Type: "document", Source: &anthropicImageSource{Type: "base64", MediaType: "application/pdf", Data: "D"}}
	areq := &anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{img, doc})},
		{Role: "assistant", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "ok"}})},
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "now"}})},
	}}
	processImages(context.Background(), areq, newImageFetcher(http.DefaultClient))
	blocks, _ := parseContentBlocks(areq.Messages[0].Content)
	texts := map[string]bool{}
	for _, b := range blocks {
		if b.Type == "text" {
			texts[b.Text] = true
		}
	}
	if !texts["\n[image omitted]\n"] {
		t.Errorf("missing [image omitted] placeholder; got %v", texts)
	}
	if !texts["\n[document omitted]\n"] {
		t.Errorf("missing [document omitted] placeholder; got %v", texts)
	}
}

// A document nested in a history tool_result (e.g. a PDF returned by a tool) is
// dropped too — the document branch of trimToolResultImages.
func TestProcessImagesTrimsToolResultDocument(t *testing.T) {
	doc := anthropicContentBlock{Type: "document", Source: &anthropicImageSource{Type: "base64", MediaType: "application/pdf", Data: "TOOL-PDF"}}
	areq := &anthropicRequest{Messages: []anthropicMessage{
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{
			{Type: "tool_result", ToolUseID: "t1", Content: mustBlocks(t, []anthropicContentBlock{doc})},
		})},
		{Role: "assistant", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "ok"}})},
		{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{{Type: "text", Text: "now"}})},
	}}
	processImages(context.Background(), areq, newImageFetcher(http.DefaultClient))
	tr, _ := parseContentBlocks(areq.Messages[0].Content)
	if len(tr) != 1 || tr[0].Type != "tool_result" {
		t.Fatalf("tool_result block lost: %+v", tr)
	}
	nested, _ := parseContentBlocks(tr[0].Content)
	for _, b := range nested {
		if b.Type == "document" {
			t.Errorf("nested tool_result document was not dropped: %+v", b)
		}
	}
}
