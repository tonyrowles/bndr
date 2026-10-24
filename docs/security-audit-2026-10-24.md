# bndr security audit

Date: October 24, 2026. Baseline: commit `778bde7` and the published Linux x86-64 `v0.2.0` archive. The audit covers discovery, dashboard APIs, proxying, preview execution, settings persistence, the installer, and release automation.

**bndr remains suitable only for trusted networks and trusted app content.** The audited `v0.2.0` release accepts untrusted HTTP Host values and was built with an outdated Go toolchain. Local fixes address those issues and add browser and HTTP defenses. At the initial audit’s completion, fixes were uncommitted and unreleased and the running `v0.2.0` instance had not been replaced. The follow-up below records the subsequent local deployment.

## Release status

The fixes described as local below are included in the `v0.2.1` security release changes: Host validation, dashboard HTTP/browser hardening, preview request filtering and its disable option, updated Go toolchain, pinned release actions, and vulnerability scan gates. The audit and follow-up sections preserve the findings and verification history before publication. Authentication and HTTPS remain deferred; bndr still requires a trusted LAN. Older release assets are unchanged.

## Findings

| Priority | Finding | Status |
| --- | --- | --- |
| High | Dashboard and bridges accept attacker-controlled Host values | Fixed locally with authority validation and regression tests |
| High | Published binary contains Go standard-library versions flagged by the vulnerability database | Local toolchain and release checks updated; published binary remains affected |
| High in an untrusted network | Discovered HTTP services are exposed without authentication | Open architectural risk; existing trusted-LAN behavior |
| Medium | Screenshot pages can navigate to unrelated services on the dev machine | Browser filtering implemented locally after the initial audit; OS isolation remains open |
| Medium | Dashboard request bodies have no total read deadline | Fixed locally with dashboard-specific I/O deadlines |
| Low | Dashboard lacks framing, resource embedding, and script restrictions | Defense in depth added locally |
| Low | Release actions use mutable major-version tags | Pinned locally to reviewed commit IDs |

### Untrusted HTTP Host values

Before the fix, an ordinary request to `/api/apps` with `Host: audit-rebinding.invalid:4000` returned HTTP 200. An isolated regression fixture also accepted a PATCH with that Host and the matching attacker Origin, changing its temporary preferences file. A bridge accepted the same forged authority and forwarded the request to its loopback target with `Host: localhost:<port>`.

These are DNS-rebinding primitives: checking Origin against an arbitrary Host does not establish a trusted origin. A full browser attack additionally depends on the victim visiting attacker-controlled content and the browser permitting the network access. No external attack infrastructure or real user-setting changes were used.

The local fix accepts only the configured LAN IP and the correct listener port, with the standard HTTP port omission allowed for port 80. It applies before dashboard routing and before bridge forwarding, including upgrade handling. Custom DNS aliases are deliberately rejected. Applications that were already listening directly on the LAN retain their own security behavior.

Evidence: `TestDashboardRejectsUntrustedHost` and `TestBridgeRejectsUntrustedHost` failed against the baseline and pass with the fix. Existing HTTP and WebSocket forwarding tests still pass.

### Published Go toolchain

The downloaded `v0.2.0` binary identifies its build toolchain as Go 1.25.14. `govulncheck v1.9.0 -mode=binary` flags 13 standard-library advisories:

`GO-2026-6809`, `GO-2026-6617`, `GO-2026-6613`, `GO-2026-6612`, `GO-2026-6611`, `GO-2026-6610`, `GO-2026-6609`, `GO-2026-6608`, `GO-2026-6607`, `GO-2026-6605`, `GO-2026-6603`, `GO-2026-6600`, and `GO-2026-6599`.

This is a binary symbol scan, not proof that every advisory is reachable or exploitable through bndr. For example, the dashboard does not enable a TLS/HTTP2 server, and its settings API does not parse multipart forms. Relevant official advisory descriptions include [CONNECT response handling](https://pkg.go.dev/vuln/GO-2026-6613), [MIME memory bounds](https://pkg.go.dev/vuln/GO-2026-6608), and [certificate distrust](https://pkg.go.dev/vuln/GO-2026-6809).

No third-party-module vulnerabilities were reported. The current source scan with local Go 1.27.3 was clean, which would have missed the release's older standard library without the separate artifact scan.

The local build now requires at least Go 1.26.10 and pins Go 1.27.3 as its toolchain. The release workflow scans both source and the generated x86-64 binary before publication. A candidate binary built with Go 1.27.3 also scans clean. Existing release assets are unchanged and need a new security release; changing Go on a user's machine does not patch a downloaded static binary.

### Automatic exposure without authentication

Any client that can reach the bound interface can enumerate apps, obtain their screenshots, change display settings, and access bndr-managed bridges. The underlying local services may include administrative or agent endpoints that rely on loopback binding rather than authentication. Rewriting Host to localhost also means downstream Host checks do not protect these deliberately exposed services.

This is the current product model rather than an accidental authentication bypass. Hiding a card is cosmetic. `BNDR_EXCLUDE` prevents bndr from creating an exposure for a selected port, but cannot close a server that already listens on the LAN itself. The first non-loopback interface is not necessarily a private or trusted LAN: VPN and public-facing interfaces need explicit configuration and firewall controls.

For broader distribution, prioritize explicit per-app exposure approval or an inclusion list, client/network access control, and authentication that covers every bridge. Adding a login only to the dashboard would leave the application ports exposed. HTTP also provides no confidentiality from an on-path observer; a remote-access design needs encrypted transport for the dashboard and app traffic.

### Screenshot network access

A controlled HTML fixture used JavaScript navigation to move from its own loopback port to a separate local-only fixture. The captured title became `AUDIT_PRIVATE_FIXTURE`. This confirms that a preview can expose content from an unrelated service through its resulting image or title. Cookies and localStorage isolation do not isolate network access.

The prerequisite is control of content loaded by a discovered page, which can include served untrusted files or compromised external scripts. The reproduction used temporary test servers and no private user data.

The new local `BNDR_PREVIEWS=0` setting prevents capture scheduling entirely. It is a mitigation, not a network sandbox. Browser request filtering has since been implemented as described below; OS-enforced network isolation is still needed for a stronger boundary against untrusted content. The running local instance was started with `BNDR_CHROMIUM_NO_SANDBOX=1` for this container; that weakens process isolation further and should be avoided on a normal host. Browser engine vulnerabilities were not assessed by the Go dependency scan.

### HTTP and browser hardening

The dashboard previously limited header-read time but not time spent reading the settings body. A client could send a valid Content-Length and drip bytes indefinitely, holding a handler and connection open despite the 2 KiB size limit. The local dashboard server now applies 15-second total read/write deadlines and a 32 KiB header limit. Bridges retain streaming and WebSocket support, with bounded headers and idle keep-alive time rather than a blanket stream deadline. There is still no global connection or per-client rate limit.

The local dashboard also sets a hash-based Content Security Policy for its embedded scripts and styles, forbids framing and cross-origin resource embedding, and suppresses referrers. These are defense-in-depth changes; the review did not find app titles or custom names being inserted as executable HTML. Upstream application responses are not given the dashboard's policy, because each app has its own content requirements.

### Installer and release trust

The installer quotes arguments, uses temporary paths, verifies the selected archive before replacing the binary, and preserves configuration. Existing tests cover invalid checksums, upgrades, special-character paths, service configuration, and uninstall behavior. No shell injection was identified in the reviewed paths.

SHA-256 hashes and binaries come from the same GitHub release. They detect corruption or mismatched files, but are not an independent signature against compromise of the publishing account or workflow. Signed attestations and provenance verification remain future hardening. The local workflow pins its checkout and Go setup actions to exact commits and adds vulnerability gates; these workflow changes have not been run on GitHub.

## Verification and limits

Completed checks:

- Baseline forged-Host request against the running server: HTTP 200.
- Isolated baseline regression tests: forged Host accepted on the dashboard and a bridge; a same-origin-looking PATCH changed only temporary fixture settings.
- Local security tests: Host rejection, headers, bounded dashboard I/O configuration, and preview disabling pass.
- Real Chromium check: dashboard startup, styles, and theme switching work under the new CSP.
- Existing discovery, HTTP, WebSocket, persistence, installer, and browser tests pass under the race detector.
- `go vet ./...`, shell syntax checks, and `git diff --check` pass.
- Source and newly built candidate binary vulnerability scans report no findings; the published binary scan reports the 13 standard-library advisories above.
- A targeted text search found no recognized GitHub-token, AWS access-key, or private-key patterns. This is not a full secret-history or screenshot-content audit.

This is a source/configuration review with controlled local reproductions. It does not include a full browser-engine audit, sustained resource-exhaustion testing, ARM runtime testing, or a complete exploit chain against a real victim browser. At initial audit completion, the local fixes had not been committed, pushed, released, or installed over the running server. The follow-up below records the subsequent local deployment.

## Follow up on preview request filtering

After the audit, browser-level filtering was implemented locally at the user's request. The target must be an HTTP(S) URL with a literal IP address and no credentials. Every intercepted request and redirect is checked against that exact scheme, normalized IP address, and port. A blocked top-level navigation returns an error rather than a screenshot from the new origin.

Document responses are fulfilled with an additional Content Security Policy before their scripts run. The added policy limits resources and connections to the app origin and disables workers, popups, forms, plugins and downloads. The app's own enforcing CSP remains in force; document reporting endpoints are removed for preview rendering. Downloads are also denied through the browser context, and service-worker bypass is enabled. Intercepted documents are read in bounded chunks with an 8 MiB maximum. Setup and interception failures abort the capture rather than falling back to unrestricted rendering.

Real-browser tests use a separate local server to count prohibited destination requests. Tests confirm zero requests for off-origin fetch/XHR, images, scripts, styles, iframe loads, beacons, WebSocket handshakes, worker fetches and popups. They also cover HTTP redirects, JavaScript navigation, meta refresh, same-origin redirects, same-origin child frames and APIs, retained app CSP, blocked document violation reports, gzip decoding, document size limits, capture cleanup, and storage isolation.

The filtering is intentionally narrower than normal browser navigation: external assets and worker-dependent rendering may be missing from thumbnails. Normal LAN app responses are unchanged. This does not establish an OS network sandbox, constrain every browser-internal protocol such as WebRTC, or contain browser-engine exploits. The unauthenticated exposure finding also remains open. The fixes are not included in any published release.

The local development instance was subsequently rebuilt as `dev-security` with Go 1.27.3 and restarted with the audit fixes and request filter. The live dashboard and a filtered sample thumbnail were checked; forged Host requests now return HTTP 403. Source and rebuilt binary vulnerability scans are clean. These changes remain uncommitted and unpublished; released `v0.2.0` downloads remain unchanged. The local container still uses the explicitly enabled no-sandbox option, so request filtering must not be treated as OS process or network isolation.
