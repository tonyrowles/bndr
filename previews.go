package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

type previewInfo struct {
	Status  string  `json:"status"`
	Version int64   `json:"version"`
	Title   string  `json:"title"`
	Error   *string `json:"error"`
}
type previewEntry struct {
	previewInfo
	url     string
	image   []byte
	updated time.Time
}
type previews struct {
	mu         sync.RWMutex
	entries    map[int]*previewEntry
	wake       chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	executable string
}

func findChromium() string {
	if path := os.Getenv("BNDR_CHROMIUM"); path != "" {
		return path
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable", "headless_shell"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	// Reuse browser downloads from the previous Node implementation if present.
	home, _ := os.UserHomeDir()
	cache := os.Getenv("PLAYWRIGHT_BROWSERS_PATH")
	if cache == "" {
		cache = filepath.Join(home, ".cache", "ms-playwright")
	}
	for _, pattern := range []string{"chromium-*/chrome-linux64/chrome", "chromium-*/chrome-linux/chrome", "chromium_headless_shell-*/chrome-headless-shell-linux64/chrome-headless-shell"} {
		paths, _ := filepath.Glob(filepath.Join(cache, pattern))
		sort.Sort(sort.Reverse(sort.StringSlice(paths)))
		for _, path := range paths {
			if stat, err := os.Stat(path); err == nil && !stat.IsDir() {
				return path
			}
		}
	}
	return ""
}
func newPreviews(parent context.Context) *previews {
	ctx, cancel := context.WithCancel(parent)
	p := &previews{entries: make(map[int]*previewEntry), wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel, done: make(chan struct{}), executable: findChromium()}
	go p.run()
	return p
}
func (p *previews) sync(apps []app) {
	p.mu.Lock()
	active := make(map[int]bool)
	for _, app := range apps {
		if app.Error != nil || !app.HTML || app.Hidden {
			continue
		}
		active[app.Port] = true
		if p.entries[app.Port] == nil {
			p.entries[app.Port] = &previewEntry{previewInfo: previewInfo{Status: "queued"}, url: app.URL}
		}
	}
	for port := range p.entries {
		if !active[port] {
			delete(p.entries, port)
		}
	}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}
func (p *previews) info(port int) previewInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if entry := p.entries[port]; entry != nil {
		return entry.previewInfo
	}
	return previewInfo{Status: "unavailable"}
}
func (p *previews) image(port int) []byte {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if entry := p.entries[port]; entry != nil {
		return entry.image
	}
	return nil
}
func (p *previews) next() (int, *previewEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ports := make([]int, 0, len(p.entries))
	for port := range p.entries {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	for _, port := range ports {
		entry := p.entries[port]
		if time.Since(entry.updated) >= time.Minute {
			entry.Status = "capturing"
			return port, entry
		}
	}
	return 0, nil
}
func (p *previews) run() {
	defer close(p.done)
	var browser context.Context
	var stopBrowser, stopAllocator context.CancelFunc
	defer func() {
		if stopBrowser != nil {
			stopBrowser()
		}
		if stopAllocator != nil {
			stopAllocator()
		}
	}()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-p.wake:
		}
		for p.ctx.Err() == nil {
			port, entry := p.next()
			if entry == nil {
				break
			}
			var image []byte
			var title string
			var err error
			if p.executable == "" {
				err = fmt.Errorf("Chromium not found; install Chromium or set BNDR_CHROMIUM")
			} else {
				if browser == nil {
					opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(p.executable), chromedp.WSURLReadTimeout(10*time.Second))
					if os.Getenv("BNDR_CHROMIUM_NO_SANDBOX") == "1" {
						opts = append(opts, chromedp.NoSandbox)
					}
					var allocator context.Context
					allocator, stopAllocator = chromedp.NewExecAllocator(p.ctx, opts...)
					browser, stopBrowser = chromedp.NewContext(allocator)
					startupCancel := stopBrowser
					watchdog := time.AfterFunc(12*time.Second, startupCancel)
					_, err = chromedp.Run(browser, chromedp.Sleep(0))
					watchdog.Stop()
					if err != nil {
						stopBrowser()
						stopAllocator()
						browser = nil
						stopBrowser = nil
						stopAllocator = nil
					}
				}
				if err == nil {
					image, title, err = capture(browser, entry.url)
					// Re-create a browser if it exits; a single slow app does not kill it.
					if chromedp.FromContext(browser).Browser != nil {
						select {
						case <-chromedp.FromContext(browser).Browser.LostConnection:
							stopBrowser()
							stopAllocator()
							browser = nil
							stopBrowser = nil
							stopAllocator = nil
						default:
						}
					}
				}
			}
			p.mu.Lock()
			if p.entries[port] == entry {
				entry.updated = time.Now()
				entry.Version = entry.updated.UnixMilli()
				if err == nil {
					entry.image = image
					entry.Title = cleanTitle(title)
					entry.Status = "ready"
					entry.Error = nil
				} else {
					message := "Preview could not be captured"
					if p.executable == "" {
						message = err.Error()
					}
					entry.Error = &message
					entry.Status = "unavailable"
					if len(entry.image) > 0 {
						entry.Status = "ready"
					}
				}
			}
			p.mu.Unlock()
		}
	}
}
func capture(browser context.Context, url string) ([]byte, string, error) {
	tab, closeTab := chromedp.NewContext(browser, chromedp.WithNewBrowserContext())
	defer closeTab()
	ctx, cancel := context.WithTimeout(tab, 12*time.Second)
	defer cancel()
	if _, err := chromedp.Run(ctx, chromedp.EmulateViewport(1200, 750)); err != nil {
		return nil, "", err
	}
	if _, err := chromedp.Run(ctx, chromedp.Func(func(ctx context.Context, target *chromedp.Target) error {
		eventCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		events := cdp.Events(eventCtx, target, page.DomContentEventFired)
		response, err := cdp.Call(ctx, target, page.Navigate, page.NavigateParams{URL: url})
		if err != nil {
			return err
		}
		if response.ErrorText != "" {
			return fmt.Errorf("navigation: %s", response.ErrorText)
		}
		for _, err := range events {
			return err
		}
		return ctx.Err()
	})); err != nil {
		return nil, "", err
	}
	if _, err := chromedp.Run(ctx, chromedp.Sleep(700*time.Millisecond)); err != nil {
		return nil, "", err
	}
	title, err := chromedp.Run(ctx, chromedp.Title())
	if err != nil {
		return nil, "", err
	}
	image, err := chromedp.Run(ctx, chromedp.Action[[]byte](func(ctx context.Context, t *chromedp.Target) ([]byte, error) {
		quality := int64(65)
		result, err := cdp.Call(ctx, t, page.CaptureScreenshot, page.CaptureScreenshotParams{Format: page.CaptureScreenshotFormatJpeg, Quality: &quality})
		return result.Data, err
	}))
	return image, title, err
}
func (p *previews) Close() { p.cancel(); <-p.done }
