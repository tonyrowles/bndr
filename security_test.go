package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardRejectsUntrustedHost(t *testing.T) {
	prefs, err := loadPreferences(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newApplication(context.Background(), "192.168.1.50", 4000, nil, prefs)
	defer a.Close()
	a.apps = []app{{Port: 8765}}
	for _, path := range []string{"/", "/api/apps", "/api/apps/8765/preview"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = "audit-rebinding.invalid:4000"
		response := httptest.NewRecorder()
		a.ServeHTTP(response, req)
		if response.Code != 403 {
			t.Errorf("forged Host on %s returned %d, want 403", path, response.Code)
		}
	}
	req := httptest.NewRequest("PATCH", "/api/apps/8765", strings.NewReader(`{"name":"attacker-controlled"}`))
	req.Host = "audit-rebinding.invalid:4000"
	req.Header.Set("Origin", "http://audit-rebinding.invalid:4000")
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	a.ServeHTTP(response, req)
	if response.Code != 403 {
		t.Errorf("rebound same-origin PATCH returned %d, want 403", response.Code)
	}
	if prefs.get(8765).Name != "" {
		t.Fatal("untrusted host changed persistent preferences")
	}
}
func TestBridgeRejectsUntrustedHost(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "private fixture") }))
	defer upstream.Close()
	port := localPort(t, upstream)
	b, err := newBridge("127.0.0.2", "127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.2:%d", port), nil)
	req.Host = fmt.Sprintf("audit-rebinding.invalid:%d", port)
	response, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatalf("bridge accepted forged Host: %d", response.StatusCode)
	}
}

func TestDashboardSecurityHeadersAndTimeouts(t *testing.T) {
	prefs, err := loadPreferences(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newApplication(context.Background(), "192.168.1.50", 4000, nil, prefs)
	defer a.Close()
	response := api(t, a, "GET", "/", "")
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
	for name, want := range map[string]string{"X-Frame-Options": "DENY", "Cross-Origin-Resource-Policy": "same-origin", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer"} {
		if got := response.Header().Get(name); got != want {
			t.Errorf("%s: got %q", name, got)
		}
	}
	policy := response.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "frame-ancestors 'none'") || !strings.Contains(policy, "script-src 'sha256-") || strings.Contains(policy, "unsafe-inline") {
		t.Fatal(policy)
	}
	// The dashboard handles bounded JSON, while bridge bodies and streams keep
	// their existing semantics; only the dashboard applies total I/O deadlines.
	server := dashboardServer(a)
	if server.ReadTimeout <= 0 || server.WriteTimeout <= 0 {
		t.Fatal("dashboard body/response I/O is unbounded")
	}
	for _, test := range []struct {
		host string
		port int
		want bool
	}{
		{"192.168.1.50:4000", 4000, true}, {"192.168.1.50", 4000, false}, {"192.168.1.50", 80, true}, {"192.168.1.50:80", 80, true}, {"attacker.invalid:4000", 4000, false}, {"192.168.1.50:4001", 4000, false},
	} {
		if got := validHost(test.host, "192.168.1.50", test.port); got != test.want {
			t.Errorf("validHost(%s,%d) = %v", test.host, test.port, got)
		}
	}
}
func TestPreviewDisableStopsCaptureScheduling(t *testing.T) {
	t.Setenv("BNDR_PREVIEWS", "0")
	p := newPreviews(context.Background())
	defer p.Close()
	p.sync([]app{{Port: 8765, HTML: true, URL: "http://127.0.0.1:8765"}})
	info := p.info(8765)
	if info.Status != "unavailable" || info.Error == nil || *info.Error != "Previews disabled" {
		t.Fatalf("disabled previews reported %+v", info)
	}
	p.mu.RLock()
	count := len(p.entries)
	p.mu.RUnlock()
	if count != 0 {
		t.Fatal("disabled preview scheduled a page capture")
	}
}
