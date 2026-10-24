package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	cdpIO "github.com/chromedp/cdproto/io"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

var errPreviewNavigationBlocked = errors.New("preview navigation outside the app origin was blocked")

type previewOrigin struct{ scheme, host, port string }

func parsePreviewOrigin(raw string) (previewOrigin, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return previewOrigin{}, errors.New("preview requires an HTTP(S) URL without credentials")
	}
	// Discovery supplies a literal interface address, never a hostname. Do not
	// introduce DNS resolution (and rebinding) into the capture allowlist.
	address, err := netip.ParseAddr(u.Hostname())
	if err != nil || address.Zone() != "" {
		return previewOrigin{}, errors.New("preview requires a literal IP address")
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return previewOrigin{}, errors.New("invalid preview port")
	}
	return previewOrigin{u.Scheme, address.String(), strconv.Itoa(number)}, nil
}
func (o previewOrigin) allows(raw string) bool {
	candidate, err := parsePreviewOrigin(raw)
	return err == nil && candidate == o
}
func (o previewOrigin) String() string { return o.scheme + "://" + net.JoinHostPort(o.host, o.port) }
func (o previewOrigin) policy() string {
	// This extra policy intersects with the app's own CSP. Only the preview gets
	// it. Workers and popups are disabled because they can create other CDP targets.
	origin := o.String()
	return "default-src " + origin + "; script-src " + origin + " 'unsafe-inline' 'unsafe-eval'; style-src " + origin + " 'unsafe-inline'; img-src " + origin + " data: blob:; font-src " + origin + " data:; media-src " + origin + " data: blob:; connect-src " + origin + "; frame-src " + origin + "; worker-src 'none'; object-src 'none'; base-uri " + origin + "; form-action 'none'; sandbox allow-scripts allow-same-origin"
}
func previewResponseHeaders(original []*fetch.HeaderEntry, policy string) []*fetch.HeaderEntry {
	result := make([]*fetch.HeaderEntry, 0, len(original)+1)
	for _, header := range original {
		switch strings.ToLower(header.Name) {
		case "content-security-policy-report-only", "report-to", "reporting-endpoints", "nel":
			continue
		case "content-security-policy":
			// Keep the app's enforcing directives, but don't send violation reports
			// to a separate endpoint while rendering a thumbnail.
			var policies []string
			for _, policy := range strings.Split(header.Value, ",") {
				var directives []string
				for _, directive := range strings.Split(policy, ";") {
					fields := strings.Fields(directive)
					if len(fields) > 0 && !strings.EqualFold(fields[0], "report-uri") && !strings.EqualFold(fields[0], "report-to") {
						directives = append(directives, directive)
					}
				}
				if len(directives) > 0 {
					policies = append(policies, strings.Join(directives, ";"))
				}
			}
			if len(policies) > 0 {
				result = append(result, &fetch.HeaderEntry{Name: header.Name, Value: strings.Join(policies, ",")})
			}
		default:
			result = append(result, header)
		}
	}
	return append(result, &fetch.HeaderEntry{Name: "Content-Security-Policy", Value: policy})
}

type previewRequestFilter struct {
	origin    previewOrigin
	mainFrame cdp.FrameID
	ctx       context.Context
	stop      context.CancelFunc
	abort     context.CancelFunc
	done      chan struct{}
	mu        sync.Mutex
	err       error
}

func (f *previewRequestFilter) fail(err error) {
	f.mu.Lock()
	if f.err == nil {
		f.err = err
	}
	f.mu.Unlock()
	f.abort()
}
func (f *previewRequestFilter) close() error {
	f.stop()
	<-f.done
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}
func installPreviewFilter(ctx context.Context, t *chromedp.Target, origin previewOrigin, abort context.CancelFunc) (*previewRequestFilter, error) {
	tree, err := cdp.Call(ctx, t, page.GetFrameTree, cdp.Empty{})
	if err != nil {
		return nil, err
	}
	eventCtx, stop := context.WithCancel(ctx)
	f := &previewRequestFilter{origin: origin, mainFrame: tree.FrameTree.Frame.ID, ctx: eventCtx, stop: stop, abort: abort, done: make(chan struct{})}
	events := cdp.Events(eventCtx, t, fetch.RequestPaused)
	go func() {
		defer close(f.done)
		for event, err := range events {
			if err == nil {
				err = f.handle(t, event)
			}
			if err != nil {
				if eventCtx.Err() == nil {
					f.fail(fmt.Errorf("preview request filter: %w", err))
				}
				return
			}
		}
		if eventCtx.Err() == nil {
			f.fail(errors.New("preview request filter stopped unexpectedly"))
		}
	}()
	fail := func(err error) (*previewRequestFilter, error) { f.close(); return nil, err }
	// Deny downloads for this isolated context, including navigations that turn
	// into attachments. Fail closed if the browser cannot install these controls.
	c := chromedp.FromContext(ctx)
	if _, err := cdp.Call(ctx, c.Browser, browser.SetDownloadBehavior, browser.SetDownloadBehaviorParams{Behavior: browser.SetDownloadBehaviorBehaviorDeny, BrowserContextID: c.BrowserContextID}); err != nil {
		return fail(err)
	}
	if _, err := cdp.Call(ctx, t, network.SetBypassServiceWorker, network.SetBypassServiceWorkerParams{Bypass: true}); err != nil {
		return fail(err)
	}
	if _, err := cdp.Call(ctx, t, fetch.Enable, fetch.EnableParams{Patterns: []*fetch.RequestPattern{
		{URLPattern: "*", RequestStage: fetch.RequestStageRequest},
	}}); err != nil {
		return fail(err)
	}
	return f, nil
}
func (f *previewRequestFilter) handle(t *chromedp.Target, event fetch.EventRequestPaused) error {
	if event.Request == nil {
		return errors.New("missing intercepted request")
	}
	if !f.origin.allows(event.Request.URL) {
		_, err := cdp.Call(f.ctx, t, fetch.FailRequest, fetch.FailRequestParams{RequestID: event.RequestID, ErrorReason: network.ErrorReasonBlockedByClient})
		if err != nil {
			return err
		}
		if event.ResourceType == network.ResourceTypeDocument && event.FrameID == f.mainFrame {
			return errPreviewNavigationBlocked
		}
		return nil
	}
	if event.ResponseErrorReason != "" {
		_, err := cdp.Call(f.ctx, t, fetch.FailRequest, fetch.FailRequestParams{RequestID: event.RequestID, ErrorReason: event.ResponseErrorReason})
		return err
	}
	if event.ResponseStatusCode != 0 {
		// Redirects have no interceptable body. Each subsequent hop is checked
		// again before it can leave the browser.
		if event.ResponseStatusCode >= 300 && event.ResponseStatusCode < 400 {
			_, err := cdp.Call(f.ctx, t, fetch.ContinueResponse, fetch.ContinueResponseParams{RequestID: event.RequestID})
			return err
		}
		return f.fulfillDocument(t, event)
	}
	// Inspect each document response before scripts run so it receives the
	// preview-only CSP, including documents reached by redirects and frames.
	var interceptResponse *bool
	if event.ResourceType == network.ResourceTypeDocument {
		enabled := true
		interceptResponse = &enabled
	}
	_, err := cdp.Call(f.ctx, t, fetch.ContinueRequest, fetch.ContinueRequestParams{RequestID: event.RequestID, InterceptResponse: interceptResponse})
	return err
}

const maxPreviewDocumentBytes = 8 << 20

func (f *previewRequestFilter) fulfillDocument(t *chromedp.Target, event fetch.EventRequestPaused) error {
	// Fulfill a fresh response so Chromium parses the added CSP before running
	// any document script. ContinueResponse header overrides do not reliably
	// re-apply document security policy in Chromium.
	stream, err := cdp.Call(f.ctx, t, fetch.TakeResponseBodyAsStream, fetch.TakeResponseBodyAsStreamParams{RequestID: event.RequestID})
	if err != nil {
		return err
	}
	defer cdp.Call(f.ctx, t, cdpIO.Close, cdpIO.CloseParams{Handle: stream.Stream})
	var body []byte
	for {
		chunk, err := cdp.Call(f.ctx, t, cdpIO.Read, cdpIO.ReadParams{Handle: stream.Stream, Size: 32 << 10})
		if err != nil {
			return err
		}
		if len(body)+len(chunk.Data) > maxPreviewDocumentBytes {
			return errors.New("preview document exceeds 8 MiB")
		}
		body = append(body, chunk.Data...)
		if chunk.EOF {
			break
		}
	}
	headers := previewResponseHeaders(event.ResponseHeaders, f.origin.policy())
	filtered := headers[:0]
	for _, header := range headers {
		// The intercepted stream is decoded. Let the fulfilled response carry
		// the actual body length, not its former compression or wire framing.
		switch strings.ToLower(header.Name) {
		case "content-encoding", "content-length", "transfer-encoding":
			continue
		}
		filtered = append(filtered, header)
	}
	filtered = append(filtered, &fetch.HeaderEntry{Name: "Content-Length", Value: strconv.Itoa(len(body))})
	_, err = cdp.Call(f.ctx, t, fetch.FulfillRequest, fetch.FulfillRequestParams{RequestID: event.RequestID, ResponseCode: event.ResponseStatusCode, ResponsePhrase: event.ResponseStatusText, ResponseHeaders: filtered, Body: body})
	return err
}
