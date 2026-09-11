# Getting Started with Loadstar

Welcome! This guide is designed to take you from a curious visitor to a Loadstar power user. Whether you're a developer wanting to track your site's performance or a friend of the author looking to see what this project is all about, you're in the right place.

---

## 1. What is Loadstar?

At its core, Loadstar is a **performance measurement engine**. Instead of relying on a single number to tell you if a site is "fast," it breaks down page speed into four distinct layers (Tiers):

1.  **Network Tier:** The "low-level" stuff. How long does it take to find the server (DNS), connect to it (TCP/TLS), and get the very first byte of data (TTFB)?
2.  **Browser Tier:** The "experience" stuff. How long until the page is actually usable? It loads the page in a real Chrome browser and tracks every single image, script, and CSS file requested (the Waterfall).
3.  **Vitals Tier:** The "Google" stuff. It extracts Core Web Vitals (LCP, FCP) directly from the browser's performance APIs.
4.  **Lighthouse Tier:** The "quality" stuff. It uses the Google PageSpeed Insights API to give you scores on Accessibility, SEO, and Best Practices.

---

## 2. Prerequisites (The Essentials)

Before you start, you'll need three things on your machine:

1.  **Go (1.26.6+):** The programming language used to build this. [Download it here](https://go.dev/dl/).
2.  **Chrome or Chromium:** Loadstar needs a real browser to run its tests. If you have Chrome installed, you're good!
3.  **A Terminal:** You'll be typing commands into your terminal (Command Prompt on Windows, Terminal on macOS/Linux).

---

## 3. Installation & Building

First, clone the project and enter the directory:

```bash
git clone https://github.com/AdrianTJ/loadstar.git
cd loadstar
```

Now, build the binary (one binary covers both the CLI and the daemon):

```bash
go build -o loadstar ./cmd/loadstar
```

---

## 4. Mode 1: The CLI (`loadstar run`)

Use this when you want to run a quick test right now.

### Basic Run
```bash
./loadstar run -u https://google.com
```

### Advanced CLI Usage
*   **Multiple Runs:** Web performance is variable. Run it 5 times to get a better average:
    ```bash
    ./loadstar run -u https://google.com -n 5
    ```
*   **Save to a Database:** Want to keep your results for later?
    ```bash
    ./loadstar run -u https://google.com -db my_results.db
    ```
*   **Specific Tiers:** Only care about Lighthouse scores?
    ```bash
    ./loadstar run -u https://google.com -t lighthouse
    ```
*   **Realistic Conditions:** By default the browser tiers run unthrottled on
    your (fast) machine. Simulate a mid-range phone on mobile data:
    ```bash
    ./loadstar run -u https://google.com -t vitals -profile slow-3g
    # profiles: none (default), 4g, fast-3g, slow-3g — browser/vitals tiers only
    ```
*   **Performance Budget (CI gate):** Fail the build when the page gets slow.
    Create `budget.yaml`:
    ```yaml
    assertions:
      network.ttfb_ms: { max: 500 }
      vitals.lcp_ms:   { max: 2500, level: warn }
    ```
    Then:
    ```bash
    ./loadstar run -u https://google.com -t all -budget budget.yaml
    # exit code 3 = an error-level assertion failed; warn-level trips exit 0
    ```

---

## 5. Mode 2: The Daemon (`loadstar serve`)

Use this if you want to build your own dashboard or integrate performance tests into a CI/CD pipeline. The daemon runs in the background and provides a REST API.

### Starting the Server
For security, the server requires an API Key. For local testing, you can bypass this:

```bash
./loadstar serve -insecure
```

### Using the API
Once the server is running, you can visit the **Interactive Documentation** at:
👉 `http://localhost:8080/docs`

You can use `curl` to submit a job:
```bash
curl -X POST http://localhost:8080/v1/jobs \
     -H "Content-Type: application/json" \
     -d '{"url": "https://example.com", "tiers": ["network", "vitals"], "runs": 1, "timeout_s": 30}'
```

- `tiers`: any of `network`, `browser`, `vitals`, `lighthouse`, or `all` (unknown names are rejected with `400`).
- `runs`: how many times to repeat the measurement (1–10).
- `timeout_s`: per-run timeout in seconds (0–600); omit or use `0` for the server default.
- `webhook_url` (optional): the daemon POSTs the final result there on completion.
- `budget` (optional): inline performance budget; the verdict shows up as
  `budget_result` on `GET /v1/jobs/{id}` and in the webhook payload. See the
  README's "Performance Budgets" section for the schema and metric keys.

- `profile` (optional): throttling profile for the browser-driven tiers
  (`none`, `4g`, `fast-3g`, `slow-3g`).

Curious how the URL has been trending? `GET /v1/history?url=https://example.com`
returns per-metric `avg`/`p50`/`p75`/`p95` — the `p75` numbers are what Google
uses to score Core Web Vitals.

Want *field* data too? Set `LOADSTAR_RUM_ORIGINS`, drop the snippet from the
README's "Real User Monitoring" section on your pages, and real visitors'
LCP/CLS/INP/FCP/TTFB will flow into `GET /v1/rum/summary?url=...` — including
INP, which lab tests fundamentally can't measure.

### Continuous Monitoring (Schedules)

Instead of poking the API yourself, let the daemon re-test on a cadence:

```bash
curl -X POST http://localhost:8080/v1/schedules \
     -H "Content-Type: application/json" \
     -d '{"url": "https://example.com", "tiers": ["network"], "interval_seconds": 300}'
```

That's a test every 5 minutes (minimum interval: 60s), starting within ~15
seconds. Each run is a normal job — it shows up in `/v1/jobs` with a
`schedule_id`, feeds `/v1/history`, and honors any `budget`/`webhook_url` you
set on the schedule. Pause with
`PATCH /v1/schedules/{id}` + `{"enabled": false}`; delete keeps the history.

Two things to set when monitoring 24/7:
- `LOADSTAR_RETENTION_DAYS=90` — trim old results so the database doesn't grow forever (default keeps everything).
- Point Prometheus at `GET /metrics` (send the `X-API-Key` header) for dashboards and alerting.

---

## 6. How it Works (Under the Hood)

If you're curious about the architecture:

*   **The Store (SQLite):** Every job and result is stored in a local file called `loadstar.db`. We use SQLite because it's fast, requires zero setup, and is incredibly reliable.
*   **The Worker Pool:** When you submit a job, it goes into a queue. A set of "Workers" (default: 4) pick up these jobs one by one. This prevents your computer from crashing if you submit 100 tests at once.
*   **Browser Reuse:** To save time and memory, we share the browser process between tests while keeping the data (cookies/cache) separate for each run.

---

## 7. Advanced: Lighthouse & API Keys

To get the most out of the **Lighthouse** tier, you should get a Google API Key (it's free).

1.  Get a key from the [Google Cloud Console](https://developers.google.com/speed/docs/insights/v5/get-started).
2.  Set it in your environment:
    ```bash
    export LOADSTAR_GOOGLE_API_KEY="your-key-here"
    ```
3.  Run a test:
    ```bash
    ./loadstar run -u https://example.com -t lighthouse
    ```

---

## 8. Troubleshooting

*   **"Chrome not found":** Ensure Chrome is installed in a standard location. Loadstar looks for `google-chrome`, `chrome`, or `chromium`.
*   **"Port 8080 already in use":** Another program is using that port. You can change it:
    ```bash
    ./loadstar serve -addr :9090
    ```
*   **SSRF Errors:** For safety, Loadstar blocks tests against `localhost` or internal IP addresses.

---

Enjoy exploring your site's performance! If you have questions, check the [`docs/`](docs/) folder for the architecture and decision log, or [`docs/openapi.yaml`](docs/openapi.yaml) for the full API spec.
