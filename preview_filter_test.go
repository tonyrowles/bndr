package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/chromedp"
)

func TestPreviewOrigin(t *testing.T) {
	origin, err := parsePreviewOrigin("http://127.0.0.1:8765/")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		url     string
		allowed bool
	}{
		{"http://127.0.0.1:8765/assets/app.js?v=1", true},
		{"http://127.0.0.1:08765/", true},
		{"http://127.0.0.1:8766/", false},
		{"https://127.0.0.1:8765/", false},
		{"http://127.0.0.2:8765/", false},
		{"http://localhost:8765/", false},
		{"http://attacker.invalid:8765/", false},
		{"http://169.254.169.254/latest/meta-data/", false},
		{"http://user:password@127.0.0.1:8765/", false},
		{"http://127.0.0.1:8765@attacker.invalid/", false},
		{"http://127.0.0.1:8765.attacker.invalid/", false},
		{"ws://127.0.0.1:8765/", false},
		{"file:///etc/passwd", false}, {"data:text/html,hello", false},
		{"blob:http://127.0.0.1:8765/id", false}, {"javascript:alert(1)", false},
	} {
		if got := origin.allows(test.url); got != test.allowed {
			t.Errorf("allows(%q)=%v", test.url, got)
		}
	}
	for _, raw := range []string{"http://127.0.0.1:0/", "http://127.0.0.1:65536/", "http://localhost/", "http://[fe80::1%25eth0]/"} {
		if _, err := parsePreviewOrigin(raw); err == nil {
			t.Errorf("accepted invalid target %q", raw)
		}
	}
	defaultPort, _ := parsePreviewOrigin("http://127.0.0.1")
	if !defaultPort.allows("http://127.0.0.1:80/") {
		t.Fatal("default port differs")
	}
	ipv6, _ := parsePreviewOrigin("http://[::1]:8765/")
	if !ipv6.allows("http://[0:0:0:0:0:0:0:1]:8765/") {
		t.Fatal("IPv6 normalization failed")
	}
}

func previewTestBrowser(t *testing.T) context.Context {
	t.Helper()
	executable := findChromium()
	if executable == "" {
		t.Skip("install Chromium for preview request tests")
	}
	parent, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	allocator, closeAllocator := chromedp.NewExecAllocator(parent, append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(executable), chromedp.NoSandbox)...)
	browser, closeBrowser := chromedp.NewContext(allocator)
	t.Cleanup(func() { closeBrowser(); closeAllocator(); cancel() })
	if _, err := chromedp.Run(browser, chromedp.Sleep(0)); err != nil {
		t.Fatal(err)
	}
	return browser
}

type requestCounts struct {
	mu    sync.Mutex
	paths map[string]int
}

func (c *requestCounts) add(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paths == nil {
		c.paths = make(map[string]int)
	}
	c.paths[path]++
}
func (c *requestCounts) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	copy := make(map[string]int)
	for path, count := range c.paths {
		copy[path] = count
	}
	return copy
}

// Avoid interference from another bndr instance watching this machine. Its
// discovery probes send localhost Host headers; the test browser uses IP URLs.
func previewFixtureHandler(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Host, "localhost:") {
			http.NotFound(w, r)
			return
		}
		handler(w, r)
	}
}

func TestPreviewBlocksOtherOriginsAndTargetEscapes(t *testing.T) {
	browser := previewTestBrowser(t)
	var forbidden, allowed requestCounts
	private := httptest.NewServer(previewFixtureHandler(func(w http.ResponseWriter, r *http.Request) {

		forbidden.add(r.URL.Path)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fmt.Fprint(w, "PRIVATE_FIXTURE")
	}))
	defer private.Close()
	privateJSON, _ := json.Marshal(private.URL)
	var app *httptest.Server
	app = httptest.NewServer(previewFixtureHandler(func(w http.ResponseWriter, r *http.Request) {

		allowed.add(r.URL.Path)
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<title>Loading</title><link rel="stylesheet" href="/style.css"><script src="/app.js" defer></script><img id="local" src="/pixel.svg"><img src="%s/image"><script src="%s/external.js"></script><link rel="stylesheet" href="%s/external.css"><iframe src="%s/frame"></iframe><iframe src="/child"></iframe><img src="/image-redirect">`, private.URL, private.URL, private.URL, private.URL)
		case "/style.css":
			w.Header().Set("Content-Type", "text/css")
			fmt.Fprint(w, "body { color: rgb(12, 34, 56) }")
		case "/pixel.svg":
			w.Header().Set("Content-Type", "image/svg+xml")
			fmt.Fprint(w, `<svg xmlns="http://www.w3.org/2000/svg" width="12" height="12"><rect width="12" height="12" fill="blue"/></svg>`)
		case "/api":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
		case "/image-redirect":
			http.Redirect(w, r, private.URL+"/redirected-image", http.StatusFound)
		case "/child":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<script>fetch(%s+'/child-fetch').catch(()=>{});new Worker('/worker.js');</script>`, privateJSON)
		case "/app.js":
			w.Header().Set("Content-Type", "application/javascript")
			fmt.Fprintf(w, `
const other=%s;
fetch(other+'/fetch').catch(()=>{});
const xhr=new XMLHttpRequest();xhr.open('POST',other+'/xhr');xhr.send('fixture');
navigator.sendBeacon(other+'/beacon','fixture');
try {new WebSocket(other.replace('http:','ws:')+'/socket')} catch {}
try {new Worker('/worker.js')} catch {}
try {new SharedWorker('/shared-worker.js')} catch {}
if(navigator.serviceWorker)navigator.serviceWorker.register('/sw.js').catch(()=>{});
try {window.open(other+'/popup')} catch {}
const child=document.createElement('iframe');child.srcdoc='<script>fetch('+JSON.stringify(other+'/srcdoc-fetch')+').catch(()=>{})<\/script>';document.body.append(child);
const blob=new Blob(['<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"/>'],{type:'image/svg+xml'});
const inline=new Image();inline.src=URL.createObjectURL(blob);document.body.append(inline);
fetch('/api').then(r=>r.json()).then(result=>{
 const image=document.querySelector('#local');
 const ready=()=>{if(result.ok&&image.naturalWidth>0&&getComputedStyle(document.body).color==='rgb(12, 34, 56)')document.title='Local assets and API rendered';};
 if(image.complete)ready();else image.onload=ready;
});`, privateJSON)
		case "/worker.js", "/shared-worker.js", "/sw.js":
			w.Header().Set("Content-Type", "application/javascript")
			fmt.Fprintf(w, `fetch(%s+'/worker-fetch')`, privateJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	defer app.Close()
	image, title, err := capture(browser, app.URL)
	if err != nil {
		t.Fatal(err)
	}
	if title != "Local assets and API rendered" {
		t.Fatalf("same-origin rendering failed: %q", title)
	}
	if _, err := jpeg.DecodeConfig(bytes.NewReader(image)); err != nil {
		t.Fatal("invalid screenshot", err)
	}
	if requests := forbidden.snapshot(); len(requests) != 0 {
		t.Fatalf("off-origin network requests reached fixture: %v", requests)
	}
	got := allowed.snapshot()
	for _, path := range []string{"/", "/style.css", "/app.js", "/api", "/pixel.svg", "/child"} {
		if got[path] == 0 {
			t.Errorf("same-origin request %s was blocked", path)
		}
	}
	for _, path := range []string{"/worker.js", "/shared-worker.js", "/sw.js"} {
		if got[path] != 0 {
			t.Errorf("worker target was allowed: %s", path)
		}
	}
	// Ordinary access to the app still has the original response and behavior.
	response, err := testClient.Get(app.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Security-Policy") != "" {
		t.Fatal("preview policy leaked into normal app traffic")
	}
}

func TestPreviewBlocksRedirectsAndScriptNavigation(t *testing.T) {
	browser := previewTestBrowser(t)
	var requests requestCounts
	private := httptest.NewServer(previewFixtureHandler(func(w http.ResponseWriter, r *http.Request) {

		requests.add(r.URL.Path)
		fmt.Fprint(w, "<title>PRIVATE_FIXTURE</title>")
	}))
	defer private.Close()
	app := httptest.NewServer(previewFixtureHandler(func(w http.ResponseWriter, r *http.Request) {

		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/http-redirect":
			http.Redirect(w, r, private.URL+"/http-escape", http.StatusFound)
		case "/script-navigation":
			fmt.Fprintf(w, `<title>Original</title><script>location.replace(%q)</script>`, private.URL+"/script-escape")
		case "/meta-navigation":
			fmt.Fprintf(w, `<title>Original</title><meta http-equiv="refresh" content="0;url=%s/meta-escape">`, private.URL)
		case "/same-origin":
			http.Redirect(w, r, "/safe", http.StatusFound)
		case "/safe":
			fmt.Fprint(w, "<title>Safe same-origin redirect</title>")
		}
	}))
	defer app.Close()
	for _, path := range []string{"/http-redirect", "/script-navigation", "/meta-navigation"} {
		t.Run(path, func(t *testing.T) {
			image, title, err := capture(browser, app.URL+path)
			if err == nil || len(image) > 0 || title != "" {
				t.Fatalf("navigation was not rejected: title=%q image=%d error=%v", title, len(image), err)
			}
			if got := requests.snapshot(); len(got) > 0 {
				t.Fatalf("blocked destination was contacted: %v", got)
			}
		})
	}
	image, title, err := capture(browser, app.URL+"/same-origin")
	if err != nil || title != "Safe same-origin redirect" || len(image) == 0 {
		t.Fatalf("same-origin redirect failed: %q %v", title, err)
	}
}

func TestPreviewPreservesAppCSPWithoutExternalReports(t *testing.T) {
	browser := previewTestBrowser(t)
	var requests requestCounts
	private := httptest.NewServer(previewFixtureHandler(func(w http.ResponseWriter, r *http.Request) {
		requests.add(r.URL.Path)
	}))
	defer private.Close()
	app := httptest.NewServer(previewFixtureHandler(func(w http.ResponseWriter, r *http.Request) {

		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'none'; report-uri "+private.URL+"/report")
		w.Header().Set("Content-Security-Policy-Report-Only", "default-src 'none'; report-uri "+private.URL+"/report-only")
		fmt.Fprint(w, `<title>Original policy preserved</title><script>document.title='Original policy weakened'</script>`)
	}))
	defer app.Close()
	_, title, err := capture(browser, app.URL)
	if err != nil || title != "Original policy preserved" {
		t.Fatalf("original CSP was weakened: %q %v", title, err)
	}
	if got := requests.snapshot(); len(got) > 0 {
		t.Fatalf("preview sent external violation reports: %v", got)
	}
}

func TestPreviewPolicyHeaders(t *testing.T) {
	origin, _ := parsePreviewOrigin("http://127.0.0.1:8765")
	original := []*fetch.HeaderEntry{{Name: "Content-Type", Value: "text/html"}, {Name: "Content-Security-Policy", Value: "script-src 'none'; report-uri https://other.invalid/report, img-src 'self'; report-to other"}, {Name: "Report-To", Value: "external reports"}, {Name: "NEL", Value: "external errors"}}
	result := previewResponseHeaders(original, origin.policy())
	var policies []string
	for _, header := range result {
		if strings.EqualFold(header.Name, "Content-Security-Policy") {
			policies = append(policies, header.Value)
		}
		if strings.Contains(header.Value, "other.invalid") {
			t.Fatal("report endpoint kept")
		}
	}
	if len(policies) != 2 || !strings.Contains(policies[0], "script-src 'none'") {
		t.Fatalf("app policy replaced: %v", policies)
	}
	if original[1].Value != "script-src 'none'; report-uri https://other.invalid/report, img-src 'self'; report-to other" {
		t.Fatal("original headers modified")
	}
}

func TestPreviewCompressedAndOversizedDocuments(t *testing.T) {
	browser := previewTestBrowser(t)
	app := httptest.NewServer(previewFixtureHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/large" {
			fmt.Fprint(w, strings.Repeat("x", maxPreviewDocumentBytes+1))
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		compressed := gzip.NewWriter(w)
		defer compressed.Close()
		fmt.Fprint(compressed, `<title>Compressed page</title><script>document.title='Compressed app rendered'</script>`)
	}))
	defer app.Close()
	image, title, err := capture(browser, app.URL)
	if err != nil || title != "Compressed app rendered" || len(image) == 0 {
		t.Fatalf("compressed document failed: %q %v", title, err)
	}
	image, title, err = capture(browser, app.URL+"/large")
	if err == nil || !strings.Contains(err.Error(), "exceeds 8 MiB") || len(image) > 0 || title != "" {
		t.Fatalf("oversized document wasn't rejected: %q %d %v", title, len(image), err)
	}
	// An aborted capture must not cancel or poison the shared browser.
	_, title, err = capture(browser, app.URL)
	if err != nil || title != "Compressed app rendered" {
		t.Fatalf("capture after rejection failed: %q %v", title, err)
	}
}
