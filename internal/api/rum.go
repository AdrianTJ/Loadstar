package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/AdrianTJ/loadstar/internal/stats"
	"github.com/AdrianTJ/loadstar/internal/store"
	"github.com/google/uuid"
)

const (
	maxRUMBody  = 8 << 10 // beacons are tiny; anything bigger is abuse
	maxRUMValue = 600000  // 10 minutes in ms — beyond any plausible vital
	// maxURLLen bounds the url label. Unlike the user agent (truncated, since a
	// clipped UA is still a usable label) an over-long url is rejected:
	// truncating would silently merge distinct pages into one bogus aggregate.
	// 2048 is the de-facto ceiling browsers enforce on URLs anyway.
	maxURLLen          = 2048
	maxUserAgentLen    = 256
	rumEventsPerSecond = 100
	rumBurst           = 200
	defaultRUMWindowH  = 24
	maxRUMWindowH      = 24 * 90
)

// rumMetrics is the allow-list of metric names the ingest endpoint accepts —
// the five metrics Google's web-vitals library reports.
var rumMetrics = map[string]bool{
	"LCP": true, "CLS": true, "INP": true, "FCP": true, "TTFB": true,
}

// tokenBucket is a minimal global rate limiter for the public ingest
// endpoint. Global (not per-IP) on purpose: per-IP is unreliable behind
// proxies, and the goal is just to bound write pressure on the store.
type tokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	max        float64
	perSecond  float64
	lastRefill time.Time
}

func newTokenBucket(perSecond, burst float64) *tokenBucket {
	return &tokenBucket{tokens: burst, max: burst, perSecond: perSecond, lastRefill: time.Now()}
}

func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens += now.Sub(b.lastRefill).Seconds() * b.perSecond
	if b.tokens > b.max {
		b.tokens = b.max
	}
	b.lastRefill = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// rumOriginAllowed reports whether the request Origin may use the endpoint.
// Requests without an Origin header (sendBeacon same-origin, curl) are
// accepted once the endpoint is enabled — CORS is a browser mechanism, not an
// auth boundary; the value/size caps and rate limit bound abuse.
func (s *Server) rumOriginAllowed(origin string) bool {
	if len(s.rumOrigins) == 0 {
		return false // endpoint disabled entirely
	}
	if origin == "" {
		return true
	}
	for _, allowed := range s.rumOrigins {
		if allowed == "*" || allowed == origin {
			return true
		}
	}
	return false
}

// setRUMCORSHeaders echoes the allowed origin for browser clients.
func (s *Server) setRUMCORSHeaders(w http.ResponseWriter, origin string) {
	if origin == "" {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
}

func (s *Server) handleRUMPreflight(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if !s.rumOriginAllowed(origin) {
		http.NotFound(w, r)
		return
	}
	s.setRUMCORSHeaders(w, origin)
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRUMIngest(w http.ResponseWriter, r *http.Request) {
	// Hide the endpoint entirely until origins are configured.
	if len(s.rumOrigins) == 0 {
		http.NotFound(w, r)
		return
	}
	origin := r.Header.Get("Origin")
	if !s.rumOriginAllowed(origin) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	if !s.rumLimiter.allow() {
		http.Error(w, "too many events", http.StatusTooManyRequests)
		return
	}

	var ev struct {
		URL   string  `json:"url"`
		Name  string  `json:"name"`
		Value float64 `json:"value"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRUMBody)
	// sendBeacon posts as text/plain; parse the body as JSON regardless of
	// Content-Type.
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "invalid event body", http.StatusBadRequest)
		return
	}

	if !rumMetrics[ev.Name] {
		http.Error(w, "unknown metric name (allowed: LCP, CLS, INP, FCP, TTFB)", http.StatusBadRequest)
		return
	}
	if ev.Value < 0 || ev.Value >= maxRUMValue {
		http.Error(w, fmt.Sprintf("value out of range [0, %d)", maxRUMValue), http.StatusBadRequest)
		return
	}
	// The url is an unauthenticated, attacker-chosen label that becomes a row
	// and an index entry, and retention defaults to keeping data forever — so
	// it has to be bounded like every other field on this endpoint.
	if len(ev.URL) > maxURLLen {
		http.Error(w, fmt.Sprintf("url exceeds %d bytes", maxURLLen), http.StatusBadRequest)
		return
	}
	// Plain URL syntax check only — validator.ValidateURL does SSRF/DNS work
	// meant for URLs the daemon fetches; RUM URLs are labels, never fetched.
	u, err := url.Parse(ev.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		http.Error(w, "url must be absolute http(s)", http.StatusBadRequest)
		return
	}

	ua := r.UserAgent()
	if len(ua) > maxUserAgentLen {
		ua = ua[:maxUserAgentLen]
	}

	if err := s.store.SaveRUMEvent(r.Context(), &store.RUMEvent{
		ID:        "rum_" + uuid.New().String(),
		URL:       ev.URL,
		Metric:    ev.Name,
		Value:     ev.Value,
		UserAgent: ua,
		CreatedAt: time.Now(),
	}); err != nil {
		http.Error(w, "failed to store event", http.StatusInternalServerError)
		return
	}

	s.setRUMCORSHeaders(w, origin)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRUMSummary(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("url")
	if target == "" {
		http.Error(w, "url query parameter is required", http.StatusBadRequest)
		return
	}
	windowH := defaultRUMWindowH
	if raw := r.URL.Query().Get("window_h"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxRUMWindowH {
			http.Error(w, fmt.Sprintf("window_h must be between 1 and %d", maxRUMWindowH), http.StatusBadRequest)
			return
		}
		windowH = n
	}

	since := time.Now().Add(-time.Duration(windowH) * time.Hour)
	values, err := s.store.GetRUMValues(r.Context(), target, since)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	metrics := make(map[string]store.MetricStats, len(values))
	for name, vs := range values {
		metrics[name] = store.MetricStats{
			Count: len(vs),
			Avg:   stats.Mean(vs),
			P50:   stats.Percentile(vs, 50),
			P75:   stats.Percentile(vs, 75),
			P95:   stats.Percentile(vs, 95),
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"url":          target,
		"window_hours": windowH,
		"metrics":      metrics,
	})
}
