package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func api(t *testing.T, a *application, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, req)
	return w
}
func TestDiscoverySettingsAndOfflineRestore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<title>Project &amp; Plans</title><h1>Hello</h1>")
	}))
	defer fixture.Close()
	port := localPort(t, fixture)
	all, err := listeners()
	if err != nil {
		t.Fatal(err)
	}
	excluded := make(map[int]bool)
	for _, s := range all {
		if s.Port != port {
			excluded[s.Port] = true
		}
	}
	dir := t.TempDir()
	prefs, err := loadPreferences(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := newApplication(ctx, "127.0.0.2", 4000, excluded, prefs)
	defer a.Close()
	a.scan(ctx)
	var data struct {
		Apps  []app `json:"apps"`
		Saved []app `json:"savedApps"`
	}
	decode := func() {
		t.Helper()
		if err := json.Unmarshal(api(t, a, "GET", "/api/apps", "").Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
	}
	decode()
	if len(data.Apps) != 1 || data.Apps[0].Name != "Project & Plans" {
		t.Fatalf("%+v", data.Apps)
	}
	url := data.Apps[0].URL
	res, err := testClient.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	path := fmt.Sprintf("/api/apps/%d", port)
	if w := api(t, a, "PATCH", path, `{"name":"My project","hidden":true}`); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	decode()
	if !data.Apps[0].Hidden || data.Apps[0].Name != "My project" {
		t.Fatal(data.Apps[0])
	}
	res, err = testClient.Get(url)
	if err != nil {
		t.Fatal("hiding removed bridge", err)
	}
	res.Body.Close()
	for _, invalid := range []string{`{"hidden":"yes"}`, `{"name":null}`, `{"hidden":null}`, `{"unexpected":true}`, `{}`, `[]`, `{} {}`} {
		if w := api(t, a, "PATCH", path, invalid); w.Code != 400 {
			t.Errorf("invalid %s returned %d", invalid, w.Code)
		}
	}
	req := httptest.NewRequest("PATCH", path, bytes.NewBufferString(`{"hidden":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://unrelated.example")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	a.Close()
	reloaded, err := loadPreferences(dir)
	if err != nil {
		t.Fatal(err)
	}
	a = newApplication(ctx, "127.0.0.2", 4000, excluded, reloaded)
	defer a.Close()
	a.scan(ctx)
	decode()
	if !data.Apps[0].Hidden || data.Apps[0].Name != "My project" {
		t.Fatal("restart lost settings")
	}
	fixture.Close()
	a.scan(ctx)
	decode()
	if len(data.Apps) != 0 || len(data.Saved) != 1 || !data.Saved[0].Offline {
		t.Fatalf("offline: %+v", data)
	}
	if w := api(t, a, "PATCH", path, `{"hidden":false,"name":""}`); w.Code != 200 {
		t.Fatal(w.Code)
	}
	decode()
	if len(data.Saved) != 0 {
		t.Fatal("offline restore failed")
	}
	if _, err := testClient.Get(url); err == nil {
		t.Fatal("stopped app left bridge open")
	}
}
func TestRealScreenshotAndBrowserIsolation(t *testing.T) {
	if findChromium() == "" {
		t.Skip("install Chromium to run screenshot integration test")
	}
	t.Setenv("BNDR_CHROMIUM_NO_SANDBOX", "1")
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<title>Initial title</title><h1>Preview</h1><script>document.title=localStorage.getItem('seen')?'Leaked storage':'Client rendered title';localStorage.setItem('seen','yes');</script>`)
	}))
	defer fixture.Close()
	p := newPreviews(context.Background())
	defer p.Close()
	port := localPort(t, fixture)
	p.sync([]app{{Port: port, HTML: true, URL: fixture.URL}})
	wait := func() {
		t.Helper()
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			info := p.info(port)
			if info.Status == "ready" {
				return
			}
			if info.Status == "unavailable" {
				t.Fatalf("capture failed using %s: %s", p.executable, *info.Error)
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("screenshot timed out")
	}
	wait()
	if p.info(port).Title != "Client rendered title" {
		t.Fatal(p.info(port))
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(p.image(port)))
	if err != nil || config.Width != 1200 || config.Height != 750 {
		t.Fatalf("screenshot %v %v", config, err)
	}
	p.mu.Lock()
	p.entries[port].updated = time.Time{}
	p.entries[port].Status = "queued"
	p.mu.Unlock()
	p.sync([]app{{Port: port, HTML: true, URL: fixture.URL}})
	wait()
	if p.info(port).Title != "Client rendered title" {
		t.Fatal("browser storage leaked between captures")
	}
	p.sync([]app{{Port: port, HTML: true, URL: fixture.URL, settings: settings{Hidden: true}}})
	if len(p.image(port)) != 0 {
		t.Fatal("hidden preview retained")
	}
}
