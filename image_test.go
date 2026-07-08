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

func TestResolveRemoteImagesRewritesURLToBase64(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(onePNGPixel)
	}))
	defer srv.Close()

	areq := &anthropicRequest{
		Messages: []anthropicMessage{
			{Role: "user", Content: mustBlocks(t, []anthropicContentBlock{
				{Type: "text", Text: "look:"},
				{Type: "image", Source: &anthropicImageSource{Type: "url", URL: srv.URL + "/x.png"}},
			})},
		},
	}

	testFetcher(srv).resolveRemoteImages(context.Background(), areq)

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
		t.Fatal("no image block after resolve")
	}
	if img.Source.Type != "base64" {
		t.Errorf("source type = %q, want base64", img.Source.Type)
	}
	if img.Source.MediaType != "image/png" {
		t.Errorf("media_type = %q, want image/png", img.Source.MediaType)
	}
	// The rewritten block must now convert to a Kiro image.
	if _, ok := convertImage(*img); !ok {
		t.Error("convertImage failed on rewritten base64 block")
	}
}

func TestResolveRemoteImagesLeavesFailedUntouched(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	original := mustBlocks(t, []anthropicContentBlock{
		{Type: "image", Source: &anthropicImageSource{Type: "url", URL: srv.URL + "/missing.png"}},
	})
	areq := &anthropicRequest{
		Messages: []anthropicMessage{{Role: "user", Content: original}},
	}

	testFetcher(srv).resolveRemoteImages(context.Background(), areq)

	blocks, _ := parseContentBlocks(areq.Messages[0].Content)
	if len(blocks) != 1 || blocks[0].Source == nil || !strings.EqualFold(blocks[0].Source.Type, "url") {
		t.Fatalf("failed download should leave url source intact, got %+v", blocks)
	}
	// A url source still downstream-skips (convertImage returns ok=false).
	if _, ok := convertImage(blocks[0]); ok {
		t.Error("convertImage should skip an unresolved url source")
	}
}

func TestResolveRemoteImagesIgnoresStringContent(t *testing.T) {
	areq := &anthropicRequest{
		Messages: []anthropicMessage{
			{Role: "user", Content: json.RawMessage(`"just text"`)},
		},
	}
	f := newImageFetcher(http.DefaultClient)
	f.resolveRemoteImages(context.Background(), areq)
	if string(areq.Messages[0].Content) != `"just text"` {
		t.Errorf("string content should be untouched, got %s", areq.Messages[0].Content)
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
