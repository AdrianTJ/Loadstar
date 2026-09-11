# Loadstar ✦

**Loadstar** is a high-performance, open-source page speed analysis toolkit written in Go. It allows developers and SREs to measure, track, and compare web performance metrics across any URL without vendor lock-in.

---

## 🚀 Key Features

- **Three-Tiered Measurement:**
  - **Network:** Sub-millisecond tracing for DNS, TCP, TLS, and TTFB using `net/http/httptrace`.
  - **Browser:** Full page load analysis and Waterfall generation via headless Chrome (`chromedp`).
  - **Vitals:** Lab web vitals via injected PerformanceObservers: LCP, FCP, CLS (proper session windowing), and TBT (the standard lab proxy for INP — real INP needs real users; see Real User Monitoring below).
- **Asynchronous Engine:** Robust job management with a configurable worker pool and state machine.
- **Dual Interface:**
  - **CLI (`loadstar run`):** Optimized for ad-hoc testing, scripts, and local developer use.
  - **API Daemon (`loadstar serve`):** A RESTful API for CI/CD integration and automated monitoring.
- **Production Ready:**
  - **Embedded Persistence:** Zero-config SQLite backend with WAL mode for high concurrency.
  - **Security:** SSRF protection and API Key authentication.
  - **Automation:** Webhook callbacks on job completion.
  - **Portability:** Multi-stage Dockerfile included.

---

## 🛠 Installation

### Prerequisites
- **Go 1.26.6+** (the version in `go.mod`; earlier 1.26 patches carry known stdlib CVEs — see `docs/security.md`)
- **Google Chrome** or **Chromium** (for browser-based tiers)

### Build from Source
```bash
go build -o loadstar ./cmd/loadstar
```

---

## 🚦 Quick Start

### CLI Mode
Perform a full performance analysis on a URL:
```bash
./loadstar run -u https://example.com -n 3 -f text
```

### API Mode
**1. Start the server (Requires API Key by default):**
```bash
export LOADSTAR_API_KEY="your-secret-key"
./loadstar serve
```
*Note: To run without a key for local testing, use `./loadstar serve -insecure`.*

**2. Submit a test job:**
```bash
curl -H "X-API-Key: your-secret-key" -X POST http://localhost:8080/v1/jobs \
  -d '{"url": "https://web.dev", "tiers": ["network"], "runs": 1, "timeout_s": 30}'
```

Request fields: `url` (required), `tiers` (any of `network`, `browser`, `vitals`,
`lighthouse`, `all`; unknown names are rejected), `runs` (1–10), `timeout_s`
(per-run seconds, 0–600; 0 uses the server default), `webhook_url` (POSTed
the final result on completion), and `budget` (see Performance Budgets below).
The daemon shuts down gracefully on `SIGINT`/`SIGTERM` (draining in-flight jobs)
and, on startup, fails any jobs left `RUNNING`/`PENDING` by a previous process
so they don't hang forever.

---

## 🎯 Performance Budgets

Budgets turn measurements into verdicts: declare thresholds on any collected
metric and every job (API or CLI) is judged `pass` / `warn` / `fail` against
them. Budgets are evaluated on the **median across runs** (so one noisy run
doesn't decide the outcome) and the verdict is stored separately from the job
status as `budget_result`.

A budget file (`budget.yaml` — JSON works too, same schema as the API's inline
`budget` object):

```yaml
assertions:
  network.ttfb_ms: { max: 500 }              # level defaults to error
  vitals.lcp_ms:   { max: 2500, level: warn } # warn: reported, doesn't fail
```

Only assert on metrics from tiers you actually run: a metric that is never
collected trips its assertion (see below), so adding e.g.
`lighthouse.performance: { min: 0.9 }` fails the budget unless the lighthouse
tier runs successfully (it needs a Google API key).

Supported metric keys: `network.ttfb_ms`, `network.total_ms`,
`network.dns_lookup_ms`, `network.tls_handshake_ms`, `network.response_bytes`,
`browser.page_load_ms`, `browser.dom_content_loaded_ms`,
`browser.resource_count`, `vitals.lcp_ms`, `vitals.fcp_ms`, `vitals.cls`,
`vitals.tbt_ms`, `lighthouse.performance`, `lighthouse.accessibility`,
`lighthouse.best_practices`, `lighthouse.seo`.

A metric that was never collected (tier failed or not requested) **trips its
assertion** at the assertion's level — missing data cannot silently pass a CI
gate. Its `actual` is reported as `null` so you can tell "missing" from "over
threshold".

**CLI (CI gate):**

```bash
./loadstar run -u https://example.com -t network -budget budget.yaml
echo $?   # 3 if an error-level assertion failed
```

CLI exit codes:

| Code | Meaning |
|---|---|
| 0 | Success (including partial tier failures and warn-level budget trips) |
| 1 | All tier attempts failed (or startup error) |
| 2 | Bad invocation: invalid tier, unreadable/invalid budget file |
| 3 | Budget violated (at least one error-level assertion failed) |

**API:** pass the same structure inline as `budget` on `POST /v1/jobs`. The
verdict appears as `budget_result` on `GET /v1/jobs/{id}` and in the webhook
payload.

## 🖥 Status Page

The daemon serves a read-only status page at `/` (e.g. `http://localhost:8080/`):
recent jobs with budget verdicts, schedules, and per-URL lab + RUM statistics.
Enter your API key once (top right); it is kept in your browser's localStorage
and sent with every request — the page itself contains no data.

By design the page is one embedded HTML file with no build step and **no write
operations** — jobs and schedules are created via the API or CLI. Trend
dashboards and alerting deliberately stay in Grafana/Alertmanager via
[`/metrics`](#-prometheus-metrics); this page exists for the quick
"is it running, did the budget pass" check.

## 🐢 Throttling Profiles

Unthrottled headless Chrome on a fast server measures your server, not your
users. Pass `profile` (API/schedules) or `-profile` (CLI) to run the
browser-driven tiers under realistic conditions (Chrome DevTools presets):

| Profile | Latency | Down | Up | CPU slowdown |
|---|---|---|---|---|
| `none` (default) | — | — | — | 1× |
| `4g` | 40ms | 10 Mbps | 10 Mbps | 1× |
| `fast-3g` | 150ms | 1.6 Mbps | 750 Kbps | 4× |
| `slow-3g` | 400ms | 400 Kbps | 400 Kbps | 4× |

```bash
./loadstar run -u https://example.com -t vitals -profile slow-3g
```

Throttling applies to the **browser and vitals tiers only** — the network tier
uses a plain Go HTTP client and always measures unthrottled, so its numbers
stay comparable across profiles.

## 📡 Real User Monitoring (RUM)

Lab numbers and field reality differ — real users have slow devices, cold
caches, and real interactions (which is also why INP can only come from the
field). Loadstar can ingest real-user beacons and aggregate them with the
same p75 statistics as lab history.

**1. Enable the endpoint** (off by default — it's a public write endpoint):

```bash
export LOADSTAR_RUM_ORIGINS="https://www.your-site.example"   # or "*"
```

**2. Add the snippet to your pages** (uses Google's `web-vitals` library):

```html
<script type="module">
  import {onLCP, onCLS, onINP, onFCP, onTTFB} from 'https://unpkg.com/web-vitals@4?module';
  const send = (m) => navigator.sendBeacon('https://YOUR-LOADSTAR-HOST/v1/rum',
    JSON.stringify({url: location.href, name: m.name, value: m.value}));
  onLCP(send); onCLS(send); onINP(send); onFCP(send); onTTFB(send);
</script>
```

**3. Query field percentiles** (authenticated):

```bash
curl -H "X-API-Key: $KEY" 'http://localhost:8080/v1/rum/summary?url=https://www.your-site.example/&window_h=24'
# {"url":"...","window_hours":24,"metrics":{"LCP":{"count":312,"p50":1810,"p75":2450,"p95":4100}, ...}}
```

Ingest guardrails: origin allow-list (CORS), 8 KB body cap, value bounds, a
global rate limit, and the same `LOADSTAR_RETENTION_DAYS` TTL as job data. Note
CORS stops browsers, not curl — treat RUM data as unauthenticated input;
aggregates (p75 over many events) are robust to noise.

## ⏱️ Scheduled Monitoring

Schedules turn one-off tests into continuous monitoring: the daemon re-tests a
URL every `interval_seconds` (minimum 60) and stores the time series.

```bash
curl -H "X-API-Key: $KEY" -X POST http://localhost:8080/v1/schedules \
  -d '{"url": "https://example.com", "tiers": ["network"], "interval_seconds": 300,
       "budget": {"assertions": {"network.ttfb_ms": {"max": 500}}}}'
```

- The first run fires on the next scheduler tick (~15s); after that, every interval.
- Schedules can carry a `budget` and a `webhook_url` — every scheduled run is
  budget-checked and webhook-notified like a manual job.
- If the previous scheduled job is still running when the next interval
  arrives, that interval is **skipped** (no pileup); the skip is counted in
  `loadstar_scheduler_runs_total{result="skipped"}`.
- Manage with `GET/PATCH/DELETE /v1/schedules[/{id}]` (PATCH toggles
  `{"enabled": bool}`). Deleting a schedule keeps its historical jobs.
- Disable the whole loop with `LOADSTAR_SCHEDULER_ENABLED=false`.

## 📊 Prometheus Metrics

`GET /metrics` exposes Prometheus text-format metrics: jobs by status, job
duration histogram, queue depth, webhook delivery results, scheduler
decisions, retention purges, and latest per-URL values (scheduled URLs only,
so label cardinality stays bounded). The endpoint requires the API key —
point Prometheus at it with a custom header:

```yaml
scrape_configs:
  - job_name: loadstar
    metrics_path: /metrics
    static_configs: [{ targets: ["loadstar-host:8080"] }]
    http_headers:
      X-API-Key:
        values: ["your-secret-key"]
```

Dashboards and alerting stay in your existing Grafana/Alertmanager — Loadstar
deliberately exports metrics instead of shipping its own dashboard UI.

## 🗑 Data Retention

`LOADSTAR_RETENTION_DAYS=90` purges completed jobs (with their results and webhook
deliveries) older than 90 days, hourly. The default is `0` — **keep
everything** — so enabling deletion is always an explicit choice. Running and
pending jobs are never purged. If you run schedules 24/7, set a retention
window so the SQLite file doesn't grow unbounded.

## 📈 History & Percentiles

`GET /v1/history?url=...` reports per-metric statistics over the recorded runs
(most recent 1000): `count`, `avg`, `p50`, `p75`, `p95`, keyed by the same
metric names budgets use. Web performance is conventionally scored at the 75th
percentile (as Google does for Core Web Vitals) — alert on `p75`, not `avg`.
The legacy top-level `avg_ttfb_ms`/`avg_total_ms` fields are kept for
compatibility.

---

## 📖 Documentation

- **[GETTING STARTED GUIDE](GETTING_STARTED.md)** (Start here!)
- **Interactive API Docs:** Visit `http://localhost:8080/docs` when the server is running to explore the API via Swagger UI.
- [Architecture](docs/architecture.md) — how it fits together and why
- [Architectural Decision Log](docs/decisions.md) — the reasoning behind each choice
- [Security Review](docs/security.md) — findings, mitigations, and deployment guidance
- [Database Query Reference](docs/database-queries.md) — inspecting `loadstar.db` directly

Testing: `go test ./...` for unit tests, `scripts/dogfood.sh` for the end-to-end
suite (add `--with-chrome` for the browser tiers), and
`scripts/dogfood_security.sh` for the security regression suite.

---

## ⚙️ Configuration

Loadstar follows a strict configuration hierarchy: **Flags > Environment Variables > `config.yaml`**.

| Env Variable | Default | Description |
|---|---|---|
| `LOADSTAR_LISTEN_ADDR` | `:8080` | API server address |
| `DATABASE_URL` | `loadstar.db` | SQLite database path |
| `LOADSTAR_API_KEY` | *(unset)* | API key for authentication |
| `LOADSTAR_WORKERS` | `4` | Number of concurrent workers (must be ≥ 1) |
| `LOADSTAR_QUEUE_DEPTH` | `256` | Job queue buffer size (must be ≥ 1) |
| `LOADSTAR_TIMEOUT_S` | `60` | Default per-run timeout in seconds; a request's `timeout_s` overrides it |
| `LOADSTAR_RETENTION_DAYS` | `0` | Purge completed jobs older than N days (0 = keep forever) |
| `LOADSTAR_SCHEDULER_ENABLED` | `true` | Set `false` to disable the recurring-monitor loop |
| `LOADSTAR_RUM_ORIGINS` | *(unset)* | Comma-separated Origin allow-list enabling the public `/v1/rum` beacon endpoint (`*` = any origin; unset = endpoint disabled) |
| `LOADSTAR_ALLOW_PRIVATE_IPS` | `false` | Allow tests/webhooks to target private/loopback IPs (local testing only) |
| `LOADSTAR_CHROME_NO_SANDBOX` | `false` | Disable the Chrome sandbox (only in trusted, isolated environments that can't support it) |

The daemon validates its numeric configuration on startup and refuses to start
with a clear error on invalid values (e.g. `LOADSTAR_WORKERS=0`, negative queue
depth) rather than hanging or panicking.

---

## 📄 License
Distributed under the MIT License. See `LICENSE` for more information.
