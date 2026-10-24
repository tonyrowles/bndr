package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

var testNetworks = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("192.0.2.0/24")}

func TestBoundAddressNetworks(t *testing.T) {
	addresses := []string{"192.168.1.50/24", "10.0.0.5/8", "100.100.1.1/32", "192.168.1.50/25"}
	got, err := networksForAddress("192.168.1.50", addresses)
	if err != nil || fmt.Sprint(got) != "[192.168.1.0/24 192.168.1.0/25]" {
		t.Fatalf("%v, %v", got, err)
	}
	if _, err := networksForAddress("192.168.2.50", addresses); err == nil {
		t.Fatal("missing address accepted")
	}
	if _, err := networksForAddress("invalid", addresses); err == nil {
		t.Fatal("invalid address accepted")
	}
	networks := []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}
	for _, tc := range []struct {
		peer string
		want bool
	}{
		{"192.168.1.10:1234", true}, {"[::ffff:192.168.1.10]:1234", true},
		{"192.168.2.10:1234", false}, {"10.0.0.5:1234", false}, {"100.100.1.1:1234", false},
		{"127.0.0.1:1234", false}, {"[::1]:1234", false}, {"bad", false},
	} {
		if got := allowedPeer(tc.peer, networks); got != tc.want {
			t.Errorf("%s: %v", tc.peer, got)
		}
	}
	if allowedPeer("192.168.1.10:1234", nil) {
		t.Fatal("empty policy must deny")
	}
	// A Tailscale /32 assignment must not authorize the entire tailnet range.
	tail, err := networksForAddress("100.100.1.1", addresses)
	if err != nil || allowedPeer("100.100.1.2:1234", tail) {
		t.Fatalf("tailnet widened: %v %v", tail, err)
	}
}

func TestClientNetworkDashboardAndBridge(t *testing.T) {
	prefs, err := loadPreferences(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	networks := []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}
	a := newApplication(context.Background(), "192.168.1.50", 4567, nil, prefs, networks)
	defer a.Close()
	for _, path := range []string{"/", "/api/apps", "/api/apps/8765/preview", "/api/apps/8765"} {
		method, body := "GET", ""
		if path == "/api/apps/8765" {
			method, body = "PATCH", `{"name":"blocked"}`
		}
		req := httptest.NewRequest(method, "http://192.168.1.50:4567"+path, strings.NewReader(body))
		req.RemoteAddr = "203.0.113.10:1234"
		req.Header.Set("Origin", "http://192.168.1.50:4567")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", "192.168.1.10")
		req.Header.Set("Forwarded", "for=192.168.1.10")
		w := httptest.NewRecorder()
		a.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatalf("%s returned %d", path, w.Code)
		}
	}
	if prefs.get(8765).Name != "" {
		t.Fatal("denied client changed settings")
	}
	req := httptest.NewRequest("GET", "http://192.168.1.50:4567/api/apps", nil)
	req.RemoteAddr = "192.168.1.10:1234"
	w := httptest.NewRecorder()
	a.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("LAN client denied: %d", w.Code)
	}
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); fmt.Fprint(w, "ok") }))
	defer upstream.Close()
	port := localPort(t, upstream)
	b, err := newBridge("127.0.0.2", "127.0.0.1", port, networks)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, upgrade := range []bool{false, true} {
		req := httptest.NewRequest("GET", fmt.Sprintf("http://127.0.0.2:%d/", port), nil)
		req.RemoteAddr = "203.0.113.10:1234"
		req.Header.Set("X-Forwarded-For", "192.168.1.10")
		if upgrade {
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
		}
		w := httptest.NewRecorder()
		b.server.Handler.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatalf("bridge upgrade=%v: %d", upgrade, w.Code)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("blocked requests reached upstream")
	}
}
