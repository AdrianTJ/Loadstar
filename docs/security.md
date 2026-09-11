# Security Review Report: Loadstar
**Date:** May 20, 2026 (updated July 7, 2026)  
**Status:** Resolved in code, except two items mitigated only at the deployment layer  
**Target:** Production-Readiness API Deployment  

---

## Executive Summary

Loadstar (then GoSpeedTest) is a robust, well-structured, and highly optimized toolkit. Parameterized queries prevent SQL injection, and default protections like local IP validation and fail-secure auth demonstrate strong security-oriented design.

A comprehensive security audit identified several critical security vulnerabilities: **blind SSRF in webhooks**, **SSRF bypasses via HTTP redirects**, **timing attacks** on API key authentication, and **Denial of Service (DoS)** vectors via unlimited iterations.

**As of July 7, 2026 all six findings have code-level mitigations** (webhook URL validation, redirect-aware clients, constant-time key comparison, the `runs` cap, DNS rebinding, and the Chrome sandbox), with test coverage to prevent regressions.

**Two mechanisms still depend partly on deployment topology and must not be treated as fully closed at the application layer:**

- **DNS rebinding for Chrome tiers (§3)** — the HTTP tiers are now protected by a connect-time IP guard (the network collector and webhook worker dial through a hardened `http.Client` whose dialer validates the resolved destination IP immediately before the socket opens). Chrome-based tiers cannot be IP-pinned without breaking TLS, so they still rely on network isolation.
- **Chrome sandbox (§5)** — the sandbox is now enabled by default in code and the container runs non-root with the setuid sandbox helper, but hosts lacking user-namespace support may fall back to the setuid helper or the `LOADSTAR_CHROME_NO_SANDBOX` opt-out; defense-in-depth network isolation is still recommended.

Below is a detailed analysis of these vulnerabilities, the mitigations applied, and their current status.

---

## 🚨 Critical Vulnerabilities (Must Fix Before Production)

### 1. SSRF in Webhooks (Bypassing Local IP Filtering)
> [!CAUTION]
> **Severity:** Critical  
> **Impact:** Remote command execution, internal system port scanning, data exfiltration.

#### The Problem:
In `internal/api/server.go`, the handler validates `req.URL` against SSRF:
```go
if err := validator.ValidateURL(req.URL); err != nil {
    http.Error(w, err.Error(), http.StatusBadRequest)
    return
}
```
However, the handler **completely fails** to validate `req.WebhookURL`! 
When a test job completes, `internal/job/manager.go` sends a POST request to `job.WebhookURL` from the server's backend:
```go
resp, err := client.Post(d.URL, "application/json", bytes.NewBuffer(d.Payload))
```
An attacker can submit a safe URL for the speed test (e.g., `https://example.com`) but set `webhook_url` to `http://127.0.0.1:8080/v1/jobs` or an cloud metadata endpoint (e.g., `http://169.254.169.254/latest/meta-data/`). This allows the attacker to launch internal HTTP requests to private endpoints, fully bypassing the SSRF protection.

#### Mitigation:
Validate `WebhookURL` using `validator.ValidateURL` before submitting the job:
```go
// internal/api/server.go
if err := validator.ValidateURL(req.URL); err != nil {
    http.Error(w, err.Error(), http.StatusBadRequest)
    return
}

if req.WebhookURL != "" {
    if err := validator.ValidateURL(req.WebhookURL); err != nil {
        http.Error(w, "invalid webhook_url: "+err.Error(), http.StatusBadRequest)
        return
    }
}
```

---

### 2. SSRF Bypass via HTTP Redirects
> [!WARNING]
> **Severity:** High  
> **Impact:** Bypassing local IP restrictions to query private/loopback services.

#### The Problem:
In `internal/collector/network/collector.go`, the network collector sends requests using `http.DefaultClient`:
```go
resp, err := http.DefaultClient.Do(req)
```
By default, Go's HTTP client automatically follows up to 10 redirects. If an attacker submits a URL pointing to `http://malicious-public-domain.com` (which resolves to a public IP and passes validation), the server will make the request. If that malicious server redirects with a `302 Found` header to `http://127.0.0.1:8080/`, the client will fetch the local resource without running `validator.ValidateURL` on the redirected URL!

#### Mitigation:
Use a custom `http.Client` for both network collection and webhook deliveries that validates redirect targets at each hop:

```go
var ssrfSafeClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		if err := validator.ValidateURL(req.URL.String()); err != nil {
			return fmt.Errorf("redirect blocked: %w", err)
		}
		return nil
	},
}
```

---

### 3. DNS Rebinding / TOCTOU SSRF
> [!WARNING]
> **Severity:** Medium-High  
> **Impact:** Bypassing local IP restrictions.
> **Status: Fixed in code (HTTP tiers), July 7, 2026.** The network collector and webhook worker now use `validator.NewSafeClient`, whose `net.Dialer.Control` hook validates the resolved destination IP at connect time (gated by `LOADSTAR_ALLOW_PRIVATE_IPS`). Because the check runs on the exact address being dialed, the rebinding TOCTOU window is closed. Chrome-based tiers still depend on network isolation.

#### The Problem:
`ValidateURL` performs a DNS lookup to check if the host resolves to a private IP, and then the HTTP client resolves it *again* to make the request. This is a classic Time-of-Check to Time-of-Use (TOCTOU) bug. An attacker can set up a custom DNS server that resolves to a public IP (e.g. `8.8.8.8`) on the first query (validation) and a private IP (e.g., `127.0.0.1`) on the second query (the actual HTTP request), with a TTL of 0.

#### Mitigation:
*   **For network requests:** Resolve the IP once during validation, and make the HTTP request directly to that IP address, setting the `Host` header to the original hostname.
*   **For Chrome/Browser tests:** Because ChromeDP cannot easily bind to a single resolved IP without breaking HTTPS/SNI certificates, the **gold standard** for production deployment is **network isolation**. Run the Loadstar daemon (and Chrome) inside a Docker container or network namespace that restricts egress access to the internal network (e.g. using `iptables` or a dedicated bridge network).

---

### 4. Authentication Timing Attacks
> [!WARNING]
> **Severity:** Medium  
> **Impact:** Character-by-character brute-forcing of the API Key.

#### The Problem:
In `internal/api/server.go`, the middleware compares the API Key using standard string comparison:
```go
key := r.Header.Get("X-API-Key")
if key != s.apiKey {
    http.Error(w, "unauthorized", http.StatusUnauthorized)
    return
}
```
Because standard `==` or `!=` comparisons return early upon finding a mismatch, the response latency varies slightly depending on how many characters of the key match. Attackers can leverage timing measurements to brute-force the API Key.

#### Mitigation:
Use constant-time comparisons using the standard library's `crypto/subtle`:
```go
import "crypto/subtle"

// ...

key := r.Header.Get("X-API-Key")
if subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) != 1 {
    http.Error(w, "unauthorized", http.StatusUnauthorized)
    return
}
```

---

### 5. Chrome Sandboxing Disabled (`--no-sandbox`)
> [!WARNING]
> **Severity:** Medium-High (Depending on host privileges)  
> **Impact:** Host compromise / Remote Code Execution (RCE) via Chrome exploits.
> **Status: Hardened, July 7, 2026 (clarified July 9 after dogfooding).** The code no longer force-disables the sandbox — `chromedp.NoSandbox` is only added when `LOADSTAR_CHROME_NO_SANDBOX=true`. **Important nuance:** chromedp auto-appends `--no-sandbox` when the process runs as **root** (`chromedp/allocate.go:159`), so "sandbox enabled by default" only holds for **non-root** execution. The real control is therefore the `Dockerfile` running as a non-root user with the setuid sandbox helper (`chrome-sandbox`, mode 4755) so the sandbox functions unprivileged. Running the daemon as root disables the sandbox regardless of the flag; the manager now logs a warning in that case (previously silent). Hosts without user-namespace support may still need the setuid helper (kept) or, as a last resort, the opt-out flag.

#### The Problem:
In `internal/chrome/manager.go`, the allocator options include:
```go
chromedp.NoSandbox,
```
Disabling the security sandbox (`--no-sandbox`) means that any zero-day exploit or vulnerability in Chrome's rendering engine (Blink/V8) triggered by a malicious site can easily escape the browser processes and execute code directly on the host machine as the user running the daemon.

#### Mitigation:
Avoid `--no-sandbox` in production. Instead:
1. Run the container as a non-privileged user.
2. In Docker, configure the container with `CAP_SYS_ADMIN` or secure user namespaces so Chrome's sandbox can function properly.

---

### 6. Denial of Service (DoS) via Unlimited Runs
> [!IMPORTANT]
> **Severity:** Medium  
> **Impact:** Server resource exhaustion, database bloat, queue starvation.

#### The Problem:
In `handleCreateJob`, the user-supplied `runs` parameter has no upper limit:
```go
if req.Runs <= 0 {
    req.Runs = 1
}
```
An attacker could submit a job requesting `runs: 100000`. Since each run spawns browser and network metrics, a single request can keep the worker pool busy indefinitely, exhaust disk space with SQLite results, and cause permanent queue starvation.

#### Mitigation:
Enforce a reasonable maximum run limit (e.g. max 10 runs per job):
```go
if req.Runs <= 0 {
    req.Runs = 1
}
if req.Runs > 10 {
    http.Error(w, "runs parameter cannot exceed 10", http.StatusBadRequest)
    return
}
```

---

## 🛠️ Security Verification Summary

| Feature | Verified Safe? | Details / Status |
|---|---|---|
| **SQL Injection** | ✅ Yes | Strictly parameterized using standard `sql.DB` placeholders (`?`). Safe. |
| **SSRF in Speed Test Target (redirects)** | ✅ Yes | **Resolved in code.** Target host validation is active and redirect-aware (`http.Client` redirects validated at each hop). |
| **DNS Rebinding / TOCTOU** | ✅ Yes (HTTP tiers) | **Resolved in code.** Connect-time IP validation via a hardened dialer (`validator.NewSafeClient`) closes the TOCTOU window. Chrome tiers still rely on network isolation. |
| **SSRF in Webhooks** | ✅ Yes | **Resolved in code.** `WebhookURL` is validated at job submission, and the background webhook worker uses a custom redirect-aware client. |
| **API Auth Strength** | ✅ Yes | **Resolved in code.** Authenticated routes are protected with `crypto/subtle.ConstantTimeCompare` key validation. |
| **Resource Exhaustion (runs)** | ✅ Yes | **Resolved in code.** Job requests hard-capped to a maximum of 10 runs per job. |
| **Chrome Sandbox** | ✅ Yes | **Hardened.** Sandbox enabled by default (opt-out via `LOADSTAR_CHROME_NO_SANDBOX`); container runs non-root with the setuid sandbox helper (see §5). |
| **Log Injection** | ✅ Yes | Uses structured logging (`slog`) with JSON formatting. |

---

## 🚀 Production Deployment Guidelines

1. **Verify Key Configuration:** Ensure a strong, highly-entropic `LOADSTAR_API_KEY` is loaded in production.
2. **Isolate Chrome Egress:** Deploy the API daemon and Chrome inside a network-isolated environment (such as a Docker container with custom network bridges) that blocks requests to private CIDRs (e.g., `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, and `169.254.169.254`) to prevent DNS rebinding in Chrome-based runs.
3. **Restrict Docker Privileges:** Run the Docker container as a non-root user and avoid running with privileged access.

---

# Second Review — July 31, 2026

**Status:** All six findings fixed in code and verified end-to-end
**Target:** Full-codebase audit (not a diff review) of Loadstar at `52243e3`
**Reproduction:** `scripts/dogfood_security.sh` — 6 passed / 16 failed before the
fixes, 22 passed / 0 failed after

A second audit covered the whole codebase rather than a changeset. The
mechanisms hardened in the first review held up: SQL is fully parameterized
(no string-built queries anywhere in `internal/store`), the status page is
XSS-clean (`createElement`/`textContent` throughout, and `urlCell` sets
`a.href = "#"` rather than the untrusted URL, so `javascript:` injection is not
possible), authentication is constant-time, and the connect-time dialer guard
genuinely closes the DNS-rebinding window.

Six new findings, in two classes the first review did not cover: **credential
handling in error paths**, and the **completeness** of the SSRF allow-list.

---

## 7. Google API Key Disclosure via Job Errors
> [!CAUTION]
> **Severity:** High
> **Impact:** Operator's Google Cloud API key disclosed to any authenticated API caller.
> **Status: Fixed, July 31, 2026.**

#### The Problem
The PageSpeed Insights key is passed as a `?key=` query parameter. Go's
transport errors embed the entire request URL in their message, and
`lighthouse.Collect` wrapped that error verbatim:

```go
q.Set("key", apiKey)
resp, err := client.Do(req)
return nil, fmt.Errorf("psi api request failed: %w", err)
// -> psi api request failed: Get "https://…/runPagespeed?key=AIzaSy…": dial tcp: …
```

That string is not internal. It flows `fail("lighthouse", err)` → `lastErr` →
`deriveStatus` (which formats `last error: %v` into the PARTIAL message) →
`store.UpdateJobStatus` → the `jobs.error` column → and out to every caller via
`GET /v1/jobs/{id}`. Any transport-level failure — a DNS blip, a timeout, a
reset connection — was enough to hand the operator's Google API key to anyone
holding a Loadstar API key.

#### Mitigation
`redactURLError` rewrites `*url.Error` to drop the query string and any
userinfo while preserving the operation, host and underlying cause, so errors
stay debuggable. Applied to both error paths that can carry the built URL.

---

## 8. SSRF Allow-List Gap: CGNAT and Other Unclassified Ranges
> [!WARNING]
> **Severity:** Medium
> **Impact:** SSRF to cloud instance metadata and internal mesh peers.
> **Status: Fixed, July 31, 2026.**

#### The Problem
`isPrivateIP` relied entirely on Go's `net.IP` predicates.
`net.IP.IsPrivate()` implements RFC 1918 (plus IPv6 ULA) and nothing else, so
several ranges used for real internal addressing passed the guard — in both
`ValidateURL` and the connect-time dialer hook, since they share the predicate:

| Address | Before | After |
|---|---|---|
| `169.254.169.254` (AWS/GCP/Azure IMDS) | blocked | blocked |
| `100.100.100.200` (**Alibaba Cloud IMDS**) | **allowed** | blocked |
| `100.64.0.1` (CGNAT / **Tailscale mesh**) | **allowed** | blocked |
| `192.0.0.0/24`, `198.18.0.0/15` | **allowed** | blocked |

On a tailnet-joined host every Tailscale peer was a valid target.

#### Mitigation
Explicit CIDR checks for RFC 6598 (`100.64.0.0/10`), RFC 6890
(`192.0.0.0/24`), RFC 2544 (`198.18.0.0/15`), and the RFC 6052 NAT64 prefix
(`64:ff9b::/96` — these embed an IPv4 address, so `64:ff9b::7f00:1` reaches
`127.0.0.1` through a translator without tripping `IsLoopback`). Tests pin the
addresses immediately outside each new CIDR as still-reachable; over-blocking
would break legitimate targets.

---

## 9. No HTTP Server Timeouts (Slowloris)
> [!WARNING]
> **Severity:** Medium
> **Impact:** Denial of service via connection exhaustion.
> **Status: Fixed, July 31, 2026.**

#### The Problem
The daemon built its server as a bare `&http.Server{Addr, Handler}`. `net/http`
applies no timeouts by default, so a client that opens connections and dribbles
headers indefinitely pins a goroutine per connection. Everything else in the
daemon is carefully bounded (runs cap, 1 MiB body cap, queue depth, per-job
timeouts), which made this the one unbounded resource.

#### Mitigation
`ReadHeaderTimeout` (10s), `ReadTimeout` (30s), `WriteTimeout` (60s) and
`IdleTimeout` (120s) via a testable `newHTTPServer` constructor. Job execution
is asynchronous and does not run on the request goroutine, so `WriteTimeout`
does not bound how long a measurement may take.

---

## 10. Unpinned Third-Party JavaScript on the API-Key Origin
> [!WARNING]
> **Severity:** Medium
> **Impact:** API key theft from `localStorage` via a compromised CDN.
> **Status: Fixed, July 31, 2026.**

#### The Problem
`/docs` is a **public** route that loaded `swagger-ui-dist` from `unpkg.com`
with no `integrity` attribute. It renders on the same origin as the status page
at `/`, which keeps the operator's API key in `localStorage`. A compromised or
hijacked CDN response would execute with full access to that key. No response
carried any security headers — there was no CSP anywhere in the codebase.

#### Mitigation
Both assets pinned with sha384 SRI hashes plus `crossorigin="anonymous"` (SRI
is not enforced without it). A `securityHeaders` middleware sets a restrictive
default CSP plus `nosniff`, `DENY` and `no-referrer`, and the two HTML routes
override the CSP with their own:

- `/docs` allows unpkg for scripts and styles, and pins its inline bootstrap by
  hash rather than allowing `unsafe-inline`.
- `/` uses a fresh per-response nonce for its inline `<style>`/`<script>`, so
  the origin holding the API key never allows `unsafe-inline`. A nonce
  generation failure is served as a 500 rather than falling back to a weaker
  policy.

A test recomputes the inline-script hash from the rendered page, so editing the
bootstrap without updating the constant fails the suite instead of silently
breaking `/docs` in the browser.

---

## 11. Truncated Identifiers Collide
> [!IMPORTANT]
> **Severity:** Medium
> **Impact:** Silent measurement loss; denial of service on the RUM endpoint.
> **Status: Fixed, July 31, 2026.**

#### The Problem
Every identifier was `uuid.New().String()[:8]` — 8 hex characters, a 2^32
space — stored in `TEXT PRIMARY KEY` columns. Simulating the generator put the
**first collision at ~159,000 ids**, consistent with the birthday bound.

The public RUM endpoint reaches that in roughly **26 minutes** at its own rate
limit of 100 events/s, after which colliding inserts fail and beacons are
rejected with a 500. Results are worse: the job manager only *logs* a failed
`SaveResult`, so a collision drops a completed measurement with no visible
error.

#### Mitigation
Full UUIDs, keeping the readable `jb_`/`res_`/`wh_`/`sc_`/`rum_`/`cli_`
prefixes. Nothing parses these ids by length and the store treats them as
opaque text.

---

## 12. Unbounded `url` Label on the Public RUM Endpoint
> [!IMPORTANT]
> **Severity:** Low
> **Impact:** Unbounded disk growth from unauthenticated input.
> **Status: Fixed, July 31, 2026.**

#### The Problem
`POST /v1/rum` is unauthenticated by design. Every field on the event was
bounded (8 KiB body, metric allow-list, value range, 256-byte user agent,
global rate limit) except the one that becomes both a row and an index entry.
A beacon could carry the better part of 8 KB of `url`, with unlimited distinct
values, into a table whose retention default is to keep everything forever.

#### Mitigation
A 2048-byte cap, rejecting rather than truncating: a clipped user agent is
still a usable label, but a clipped url silently merges distinct pages into one
meaningless aggregate.

---

## 13. Known Standard-Library Vulnerabilities via an Outdated Toolchain
> [!WARNING]
> **Severity:** Medium
> **Impact:** DoS and information disclosure through the HTTP and TLS paths.
> **Status: Fixed, July 31, 2026** — found by `govulncheck` on its first CI run.

#### The Problem
`go.mod` pinned `go 1.26.2`, and CI installs exactly the version named there, so
the build shipped a standard library three patch releases behind. `govulncheck`
reported five vulnerabilities **reachable from this code** — all in the standard
library, none in third-party dependencies:

| ID | Package | Fixed in | Why it matters here |
|---|---|---|---|
| GO-2026-5856 | `crypto/tls` — Encrypted Client Hello privacy leak | go1.26.5 | Every outbound HTTPS measurement |
| GO-2026-5039 | `net/textproto` — arbitrary input in errors without escaping | go1.26.4 | Response headers from an arbitrary target reach `jobs.error`, which the API returns |
| GO-2026-5037 | `crypto/x509` — inefficient candidate hostname parsing | go1.26.4 | A hostile certificate can burn CPU on a target we were asked to measure |
| GO-2026-4971 | `net` — panic on NUL byte in `Dial`/`LookupPort` | go1.26.3 | Reachable from `validator.ValidateURL` (Windows only) |
| GO-2026-4918 | `net/http` — infinite loop on bad `SETTINGS_MAX_FRAME_SIZE` | go1.26.3 | A malicious target could hang a worker via HTTP/2 |

These are not abstract for Loadstar. The daemon exists to fetch URLs chosen by
its users, so hostile-server bugs in the HTTP and TLS stacks are directly on the
attack surface — GO-2026-4918 in particular is a worker-hang primitive, and
GO-2026-5039 is the same shape as finding §7: untrusted bytes reaching an error
string that the API hands back to callers.

#### Mitigation
Raised the `go` directive to `1.26.5`, the lowest release that clears all five.
`README.md` and `GETTING_STARTED.md` state the requirement (the latter still
claimed 1.21). Keeping the toolchain current is now enforced rather than
remembered: `govulncheck` fails the build whenever a reachable advisory appears.

#### Recurrence — September 11, 2026
The same check caught the same class of drift two months later. The `go`
directive still named `1.26.5`, and `govulncheck` reported five more reachable
standard-library advisories, all fixed in `go1.26.6`:

| ID | Package | Fixed in | Why it matters here |
|---|---|---|---|
| GO-2026-6218 | `net/url` — quadratic complexity in `resolvePath` | go1.26.6 | `lighthouse.Collect` parses target URLs |
| GO-2026-6090 | `crypto/tls` — post-handshake message limit | go1.26.6 | Every outbound HTTPS measurement |
| GO-2026-6089 | `net/http` — `ReadHeaderTimeout` skipped in the unencrypted HTTP/2 check | go1.26.6 | The daemon's own listener (§5) |
| GO-2026-5972 | `encoding/asn1` — unbounded recursion | go1.26.6 | `validator.NewSafeClient` clones the transport |
| GO-2026-5026 | `net/http` (idna) — ASCII-only Punycode labels accepted | go1.26.6 | `lighthouse.Collect` and webhook delivery |

`go.mod` now pins `1.26.6`, with `README.md` and `GETTING_STARTED.md` following
it. The July lesson held: the guard works, and the remedy is a one-line bump as
often as the standard library needs one.

---

## Findings Reviewed and Accepted

| Observation | Disposition |
|---|---|
| Internal error strings (`err.Error()` from the store) returned to clients | Accepted. Auth-protected routes only; leaks schema hints, not data. |
| `network.Collect` discards the response body with no size cap | Accepted, unchanged from the round-2 deferral. Memory-safe (`io.Discard`) and bounded by the 30s client timeout; costs bandwidth only. |
| RUM CORS echoes the request Origin | Accepted. Checked against the allow-list first, and the endpoint is write-only returning 204 — nothing to leak. |
| `subtle.ConstantTimeCompare` returns early on length mismatch | Accepted. Leaks key length only, which is not secret. |

## Follow-Up Not Yet Done

- ~~**No `gosec` or `govulncheck` in CI.**~~ **Done, July 31, 2026.** Both now
  run on every push and pull request. `gosec` is configured with `-exclude=G104`
  because errcheck already owns unhandled-error policy via `.golangci.yml`;
  the two `G304` findings (operator-supplied `-config` and `-budget` paths) are
  annotated `#nosec` with justifications at the call sites. Verified that
  removing the timeouts from `newHTTPServer` makes `gosec` report G112 again,
  so finding §9 could not recur unnoticed.
- **Chrome-tier DNS rebinding (§3) and the Chrome sandbox (§5)** still depend
  partly on deployment topology; unchanged by this review.
