package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// Exercise the embedded page in Chromium without exposing any actual apps.
func TestDashboardThemeAndPolling(t *testing.T) {
	executable := findChromium()
	if executable == "" {
		t.Skip("install Chromium for dashboard browser tests")
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			w.Write(dashboard)
		case "/api/apps":
			fmt.Fprint(w, `{"apps":[{"port":8765,"name":"Local test app","autoName":"Local test app","customName":"","url":"http://127.0.0.1:8765","mode":"proxy","hidden":false,"preview":{"status":"unavailable"}}],"savedApps":[],"lan":"192.168.1.50","port":4000,"error":null}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fixture.Close()
	parent, deadline := context.WithTimeout(context.Background(), 40*time.Second)
	defer deadline()
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(executable), chromedp.NoSandbox)
	allocator, closeAllocator := chromedp.NewExecAllocator(parent, opts...)
	defer closeAllocator()
	browser, closeBrowser := chromedp.NewContext(allocator)
	defer closeBrowser()
	if _, err := chromedp.Run(browser, chromedp.Navigate(fixture.URL)); err != nil {
		t.Fatal(err)
	}
	if _, err := chromedp.Run(browser, chromedp.WaitVisible(chromedp.CSS(`[data-port="8765"]`))); err != nil {
		t.Fatal(err)
	}
	check := func(script string) {
		t.Helper()
		ok, err := chromedp.Run(browser, chromedp.Evaluate[bool](script, chromedp.EvalAwaitPromise))
		if err != nil || !ok {
			t.Fatalf("browser check failed: %v\n%s", err, script)
		}
	}
	check(`document.querySelector('h1').textContent==='Apps' && !document.body.textContent.includes('Your workspace.')`)
	check(`(() => { const select=document.querySelector('#theme');select.value='dark';select.dispatchEvent(new Event('change'));return document.documentElement.dataset.theme==='dark' && localStorage.getItem('bndr-theme')==='dark'; })()`)
	if _, err := chromedp.Run(browser, chromedp.Navigate(fixture.URL)); err != nil {
		t.Fatal(err)
	}
	if _, err := chromedp.Run(browser, chromedp.WaitVisible(chromedp.CSS(`[data-port="8765"]`))); err != nil {
		t.Fatal(err)
	}
	check(`document.documentElement.dataset.theme==='dark' && document.querySelector('#theme').value==='dark'`)
	check(`(async()=>{const card=document.querySelector('[data-port="8765"]');const summary=card.querySelector('summary');summary.click();summary.focus();card.dataset.test='preserved';await load();return document.querySelector('[data-port="8765"]')===card && card.querySelector('details').open && document.activeElement===summary;})()`)
	check(`(async()=>{const rename=[...document.querySelectorAll('.options button')].find(button=>button.textContent==='Rename');rename.click();const input=document.querySelector('#app-name');input.value='Unsaved name';await load();return document.querySelector('#rename-dialog').open && input.value==='Unsaved name' && document.activeElement===input;})()`)
	check(`(()=>{document.querySelector('#rename-dialog').close();const select=document.querySelector('#theme');select.value='light';select.dispatchEvent(new Event('change'));return document.documentElement.dataset.theme==='light';})()`)
	if _, err := chromedp.Run(browser, chromedp.EmulateViewport(320, 780)); err != nil {
		t.Fatal(err)
	}
	check(`document.documentElement.scrollWidth<=innerWidth`)
	check(`(()=>{const input=document.querySelector('#filter');input.value='does not match';input.dispatchEvent(new Event('input'));return document.querySelector('.empty h2').textContent==='No matching apps';})()`)
}
