package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestIsLoopbackRemote(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:1234", true},
		{"[::1]:5678", true},
		{"192.168.1.10:80", false},
		{"8.8.8.8:443", false},
		{"127.0.0.1", true}, // no port
		{"garbage", false},
	}
	for _, c := range cases {
		if got := isLoopbackRemote(c.addr); got != c.want {
			t.Errorf("isLoopbackRemote(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestLoopbackOnlyMiddleware(t *testing.T) {
	h := loopbackOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Loopback peer is allowed.
	req := httptest.NewRequest(http.MethodGet, "/api/status.json", nil)
	req.RemoteAddr = "127.0.0.1:40000"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("loopback peer: got %d, want 200", rr.Code)
	}

	// Non-loopback peer is forbidden, even with a spoofed forwarded header.
	req = httptest.NewRequest(http.MethodGet, "/api/status.json", nil)
	req.RemoteAddr = "203.0.113.5:40000"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("remote peer: got %d, want 403", rr.Code)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.1:27890", true},
		{"localhost", true},
		{"localhost:27890", true},
		{"LocalHost:27890", true},
		{"foo.localhost:27890", true},
		{"[::1]:27890", true},
		{"evil.com:27890", false},
		{"192.168.1.5:27890", false},
		{"attacker.example", false},
	}
	for _, c := range cases {
		if got := isLoopbackHost(c.host); got != c.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

func TestAdminHostOnlyMiddleware(t *testing.T) {
	h := adminHostOnly(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// A rebound attacker domain resolving to loopback would pass the peer check
	// but must be rejected on the Host header.
	req := httptest.NewRequest(http.MethodGet, "/api/status.json", nil)
	req.Host = "evil.example.com:27890"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("rebinding host: got %d, want 403", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/status.json", nil)
	req.Host = "localhost:27890"
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("localhost host: got %d, want 200", rr.Code)
	}
}

func TestListenWithAutoIncrement(t *testing.T) {
	// Occupy a port, then ask the helper to start from it; it must pick a higher
	// free port instead of failing.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy: %v", err)
	}
	defer occupied.Close()

	_, startPort, err := net.SplitHostPort(occupied.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	start, _ := strconv.Atoi(startPort)

	ln, got, err := listenWithAutoIncrement("127.0.0.1", start)
	if err != nil {
		t.Fatalf("listenWithAutoIncrement: %v", err)
	}
	defer ln.Close()

	if got <= start {
		t.Fatalf("expected a port above the occupied %d, got %d", start, got)
	}
}
