package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type app struct {
	Port     int     `json:"port"`
	Host     string  `json:"-"`
	Direct   bool    `json:"-"`
	Title    string  `json:"title"`
	HTML     bool    `json:"html"`
	Upstream string  `json:"upstream"`
	Mode     string  `json:"mode"`
	URL      string  `json:"url"`
	Error    *string `json:"error"`
	settings
	CustomName string      `json:"customName"`
	AutoName   string      `json:"autoName"`
	Preview    previewInfo `json:"preview"`
	Offline    bool        `json:"offline,omitempty"`
}
type application struct {
	lan         string
	port        int
	excluded    map[int]bool
	preferences *preferences
	previews    *previews
	scanMu      sync.Mutex
	bridges     map[int]*bridge
	mu          sync.RWMutex
	apps        []app
	lastScan    *time.Time
	scanError   *string
}

func newApplication(ctx context.Context, lan string, port int, excluded map[int]bool, prefs *preferences) *application {
	return &application{lan: lan, port: port, excluded: excluded, preferences: prefs, previews: newPreviews(ctx), bridges: make(map[int]*bridge), apps: []app{}}
}
func (a *application) scan(ctx context.Context) {
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	sockets, err := listeners()
	if err != nil {
		message := err.Error()
		a.mu.Lock()
		a.scanError = &message
		a.mu.Unlock()
		log.Printf("Discovery: %s", message)
		return
	}
	groups := make(map[int][]socket)
	for _, s := range sockets {
		if s.Port == a.port || a.excluded[s.Port] {
			continue
		}
		if a.bridges[s.Port] != nil && s.Host == a.lan {
			continue
		}
		if !slices.Contains([]string{"0.0.0.0", "::", "::1", a.lan}, s.Host) && !strings.HasPrefix(s.Host, "127.") {
			continue
		}
		groups[s.Port] = append(groups[s.Port], s)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	found := make(map[int]app)
	limit := make(chan struct{}, 32)
	for port, entries := range groups {
		wg.Go(func() {
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-limit }()
			entry := entries[0]
			direct := false
			for _, candidate := range entries {
				if candidate.Host == "0.0.0.0" || candidate.Host == a.lan {
					entry = candidate
					direct = true
					break
				}
			}
			host := entry.Host
			if host == "::" {
				host = "::1"
			}
			if host == "0.0.0.0" {
				host = "127.0.0.1"
			}
			meta, ok := probe(ctx, host, port)
			if !ok {
				return
			}
			value := app{Port: port, Host: host, Direct: direct, Title: meta.Title, HTML: meta.HTML, Upstream: fmt.Sprintf("%s:%d", host, port), URL: fmt.Sprintf("http://%s:%d", a.lan, port), Mode: "proxy"}
			if direct {
				value.Mode = "direct"
			}
			mu.Lock()
			found[port] = value
			mu.Unlock()
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	for port, bridge := range a.bridges {
		app, ok := found[port]
		if !ok || app.Direct || app.Host != bridge.host {
			bridge.Close()
			delete(a.bridges, port)
		}
	}
	next := make([]app, 0, len(found))
	for port, app := range found {
		if !app.Direct && a.bridges[port] == nil {
			bridge, err := newBridge(a.lan, app.Host, port)
			if err != nil {
				message := err.Error()
				app.Error = &message
			} else {
				a.bridges[port] = bridge
			}
		}
		app.settings = a.preferences.get(port)
		next = append(next, app)
	}
	sort.Slice(next, func(i, j int) bool { return next[i].Port < next[j].Port })
	a.previews.sync(next)
	now := time.Now().UTC()
	a.mu.Lock()
	a.apps = next
	a.lastScan = &now
	a.scanError = nil
	a.mu.Unlock()
}
func (a *application) syncPreviews() {
	a.mu.RLock()
	apps := slices.Clone(a.apps)
	a.mu.RUnlock()
	for i := range apps {
		apps[i].settings = a.preferences.get(apps[i].Port)
	}
	a.previews.sync(apps)
}
func (a *application) Close() {
	a.scanMu.Lock()
	for port, bridge := range a.bridges {
		bridge.Close()
		delete(a.bridges, port)
	}
	a.scanMu.Unlock()
	a.previews.Close()
}
func (a *application) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/api/apps" && r.Method == http.MethodGet {
		a.mu.RLock()
		apps := slices.Clone(a.apps)
		lastScan := a.lastScan
		scanError := a.scanError
		a.mu.RUnlock()
		saved := a.preferences.snapshot()
		active := make(map[int]bool)
		offline := []app{}
		for i := range apps {
			value := &apps[i]
			active[value.Port] = true
			value.settings = saved[value.Port]
			value.CustomName = value.Name
			value.Preview = a.previews.info(value.Port)
			value.AutoName = value.Preview.Title
			if value.AutoName == "" {
				value.AutoName = value.Title
			}
			if value.AutoName == "" {
				value.AutoName = fmt.Sprintf("App on %d", value.Port)
			}
			if value.Name == "" {
				value.Name = value.AutoName
			}
		}
		for port, settings := range saved {
			if settings.Hidden && !active[port] {
				name := settings.Name
				if name == "" {
					name = fmt.Sprintf("App on %d", port)
				}
				value := app{Port: port, settings: settings, Offline: true, CustomName: settings.Name}
				value.Name = name
				offline = append(offline, value)
			}
		}
		sort.Slice(offline, func(i, j int) bool { return offline[i].Port < offline[j].Port })
		writeJSON(w, map[string]any{"apps": apps, "savedApps": offline, "lan": a.lan, "port": a.port, "lastScan": lastScan, "error": scanError})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/apps/") {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/apps/"), "/")
		port, err := strconv.Atoi(parts[0])
		if err != nil || port < 1 || port > 65535 {
			http.NotFound(w, r)
			return
		}
		if len(parts) == 2 && parts[1] == "preview" && r.Method == http.MethodGet {
			image := a.previews.image(port)
			if len(image) == 0 {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(image)
			return
		}
		if len(parts) == 1 && r.Method == http.MethodPatch {
			a.updateSettings(w, r, port)
			return
		}
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", 405)
		return
	}
	if r.URL.Path == "/" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(dashboard)
		return
	}
	http.NotFound(w, r)
}
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}
func (a *application) updateSettings(w http.ResponseWriter, r *http.Request, port int) {
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
		http.Error(w, "Origin not allowed", 403)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]), "application/json") {
		http.Error(w, "Expected application/json", 415)
		return
	}
	known := false
	a.mu.RLock()
	for _, app := range a.apps {
		if app.Port == port {
			known = true
			break
		}
	}
	a.mu.RUnlock()
	if _, ok := a.preferences.snapshot()[port]; !known && !ok {
		http.NotFound(w, r)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2048))
	if err != nil {
		http.Error(w, "Request too large", 400)
		return
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil || len(fields) == 0 {
		http.Error(w, "Invalid settings", 400)
		return
	}
	var name *string
	var hidden *bool
	for key, value := range fields {
		switch key {
		case "name":
			var input string
			if string(value) == "null" || json.Unmarshal(value, &input) != nil || len([]rune(input)) > 120 {
				http.Error(w, "Name must be at most 120 characters", 400)
				return
			}
			input = strings.TrimSpace(input)
			name = &input
		case "hidden":
			var input bool
			if string(value) == "null" || json.Unmarshal(value, &input) != nil {
				http.Error(w, "Hidden must be a boolean", 400)
				return
			}
			hidden = &input
		default:
			http.Error(w, "Invalid settings", 400)
			return
		}
	}
	if err = a.preferences.update(port, name, hidden); err != nil {
		http.Error(w, "Could not save preferences", 500)
		return
	}
	a.syncPreviews()
	writeJSON(w, map[string]bool{"ok": true})
}
