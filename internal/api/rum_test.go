package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AdrianTJ/loadstar/internal/store"
)

func postRUM(mux http.Handler, body string, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/rum", bytes.NewReader([]byte(body)))
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

const goodEvent = `{"url":"https://site.example/page","name":"LCP","value":1234.5}`

func TestRUM_DisabledWithoutOrigins(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "rum-disabled", "", true, nil)
	if w := postRUM(mux, goodEvent, ""); w.Code != http.StatusNotFound {
		t.Errorf("ingest status = %d, want 404 when unconfigured", w.Code)
	}
	// Preflight is hidden too.
	req := httptest.NewRequest("OPTIONS", "/v1/rum", nil)
	req.Header.Set("Origin", "https://site.example")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("preflight status = %d, want 404 when unconfigured", w.Code)
	}
}

func TestRUM_IngestHappyPath(t *testing.T) {
	_, mux, s, _ := newTestServer(t, "rum-happy", "", true, []string{"https://site.example"})

	w := postRUM(mux, goodEvent, "https://site.example")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (%s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://site.example" {
		t.Errorf("CORS header = %q", got)
	}

	values, err := s.GetRUMValues(context.Background(), "https://site.example/page", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("get values: %v", err)
	}
	if len(values["LCP"]) != 1 || values["LCP"][0] != 1234.5 {
		t.Errorf("stored values = %v, want [1234.5]", values["LCP"])
	}
}

func TestRUM_NoOriginHeaderAccepted(t *testing.T) {
	// sendBeacon from the same origin (and curl) sends no Origin header; once
	// the endpoint is enabled those must work.
	_, mux, _, _ := newTestServer(t, "rum-noorigin", "", true, []string{"https://site.example"})
	if w := postRUM(mux, goodEvent, ""); w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 for origin-less beacon", w.Code)
	}
}

func TestRUM_DisallowedOrigin(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "rum-badorigin", "", true, []string{"https://site.example"})
	if w := postRUM(mux, goodEvent, "https://evil.example"); w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestRUM_WildcardOrigin(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "rum-wildcard", "", true, []string{"*"})
	if w := postRUM(mux, goodEvent, "https://anything.example"); w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 with wildcard", w.Code)
	}
}

func TestRUM_Preflight(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "rum-preflight", "", true, []string{"https://site.example"})
	req := httptest.NewRequest("OPTIONS", "/v1/rum", nil)
	req.Header.Set("Origin", "https://site.example")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", w.Code)
	}
	if !strings.Contains(w.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Errorf("preflight missing POST in allow-methods")
	}
}

func TestRUM_ValidationRejections(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "rum-validation", "", true, []string{"*"})
	cases := []struct {
		name string
		body string
	}{
		{"bad metric", `{"url":"https://a.example/","name":"SPEED","value":1}`},
		{"negative value", `{"url":"https://a.example/","name":"LCP","value":-1}`},
		{"huge value", `{"url":"https://a.example/","name":"LCP","value":900000}`},
		{"relative url", `{"url":"/page","name":"LCP","value":1}`},
		{"non-http scheme", `{"url":"ftp://a.example/","name":"LCP","value":1}`},
		{"not json", `LCP=1200`},
	}
	for _, c := range cases {
		if w := postRUM(mux, c.body, ""); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", c.name, w.Code)
		}
	}
}

func TestRUM_OversizeBodyRejected(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "rum-oversize", "", true, []string{"*"})
	big := `{"url":"https://a.example/","name":"LCP","value":1,"pad":"` + strings.Repeat("x", maxRUMBody) + `"}`
	if w := postRUM(mux, big, ""); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for oversize body", w.Code)
	}
}

func TestRUM_RateLimit(t *testing.T) {
	srv, mux, _, _ := newTestServer(t, "rum-ratelimit", "", true, []string{"*"})
	// Drain the bucket instantly instead of sending rumBurst real requests.
	srv.rumLimiter.mu.Lock()
	srv.rumLimiter.tokens = 0
	srv.rumLimiter.lastRefill = time.Now()
	srv.rumLimiter.mu.Unlock()

	if w := postRUM(mux, goodEvent, ""); w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 when bucket empty", w.Code)
	}
}

func TestRUM_SummaryRequiresAuthAndComputesP75(t *testing.T) {
	// Authenticated server this time (apiKey set, not insecure).
	_, mux, s, _ := newTestServer(t, "rum-summary", "secret", false, []string{"*"})

	url := "https://site.example/page"
	for i, v := range []float64{100, 200, 300, 400} {
		s.SaveRUMEvent(context.Background(), &store.RUMEvent{
			ID: "rum_" + string(rune('a'+i)), URL: url, Metric: "LCP", Value: v, CreatedAt: time.Now(),
		})
	}

	// No key -> 401.
	req := httptest.NewRequest("GET", "/v1/rum/summary?url="+url, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status without key = %d, want 401", w.Code)
	}

	// With key -> p75 = 325.
	req = httptest.NewRequest("GET", "/v1/rum/summary?url="+url, nil)
	req.Header.Set("X-API-Key", "secret")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status with key = %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		WindowHours int `json:"window_hours"`
		Metrics     map[string]struct {
			Count int     `json:"count"`
			P75   float64 `json:"p75"`
		} `json:"metrics"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.WindowHours != 24 {
		t.Errorf("window_hours = %d, want default 24", resp.WindowHours)
	}
	lcp, ok := resp.Metrics["LCP"]
	if !ok || lcp.Count != 4 || lcp.P75 != 325 {
		t.Errorf("LCP stats = %+v, want count=4 p75=325", lcp)
	}
}

func TestCreateJob_ProfileValidation(t *testing.T) {
	_, mux, s, _ := newTestServer(t, "api-profile", "", true, nil)

	// Unknown profile -> 400 listing the valid names.
	bad := `{"url":"http://example.com","tiers":["network"],"profile":"5g"}`
	req := httptest.NewRequest("POST", "/v1/jobs", bytes.NewReader([]byte(bad)))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "slow-3g") {
		t.Errorf("status = %d body=%q, want 400 listing profiles", w.Code, w.Body.String())
	}

	// Valid profile -> accepted and persisted on the job row.
	good := `{"url":"http://example.com","tiers":["network"],"profile":"slow-3g"}`
	req = httptest.NewRequest("POST", "/v1/jobs", bytes.NewReader([]byte(good)))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	var created map[string]string
	json.Unmarshal(w.Body.Bytes(), &created)
	jb, err := s.GetJob(context.Background(), created["job_id"])
	if err != nil || jb == nil {
		t.Fatalf("get job: %v", err)
	}
	if jb.Profile != "slow-3g" {
		t.Errorf("job profile = %q, want slow-3g", jb.Profile)
	}
}

func TestCreateSchedule_ProfileValidation(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "api-sched-profile", "", true, nil)

	bad := `{"url":"http://example.com","tiers":["network"],"interval_seconds":60,"profile":"warp"}`
	req := httptest.NewRequest("POST", "/v1/schedules", bytes.NewReader([]byte(bad)))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for bad schedule profile", w.Code)
	}

	good := `{"url":"http://example.com","tiers":["network"],"interval_seconds":60,"profile":"4g"}`
	req = httptest.NewRequest("POST", "/v1/schedules", bytes.NewReader([]byte(good)))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	var sc store.Schedule
	json.Unmarshal(w.Body.Bytes(), &sc)
	if sc.Profile != "4g" {
		t.Errorf("schedule profile = %q, want 4g", sc.Profile)
	}
}

func TestRUM_SummaryBadWindow(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "rum-window", "", true, []string{"*"})
	for _, q := range []string{"window_h=0", "window_h=-5", "window_h=99999", "window_h=abc"} {
		req := httptest.NewRequest("GET", "/v1/rum/summary?url=https://a.example/&"+q, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, w.Code)
		}
	}
}

// TestRUMIngest_URLLengthBounded is the regression for the July 2026 audit
// finding SEC-6. /v1/rum is a public write endpoint and the url is an
// attacker-chosen label; every other field was bounded but this one was not,
// so a beacon could carry ~8 KB of url per row into a table whose retention
// default is "keep forever".
func TestRUMIngest_URLLengthBounded(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "rum-urllen", "", true, []string{"*"})

	post := func(target string) int {
		body, err := json.Marshal(map[string]interface{}{
			"url": target, "name": "LCP", "value": 1200,
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return postRUM(mux, string(body), "").Code
	}

	long := "https://example.com/" + strings.Repeat("a", maxURLLen)
	if code := post(long); code != http.StatusBadRequest {
		t.Errorf("over-long url: status = %d, want 400", code)
	}

	// A normal beacon, and one right at the limit, must still be accepted.
	if code := post("https://example.com/page"); code != http.StatusNoContent {
		t.Errorf("normal url: status = %d, want 204", code)
	}
	atLimit := "https://example.com/" + strings.Repeat("b", maxURLLen-len("https://example.com/"))
	if len(atLimit) != maxURLLen {
		t.Fatalf("test bug: built a %d-byte url, want %d", len(atLimit), maxURLLen)
	}
	if code := post(atLimit); code != http.StatusNoContent {
		t.Errorf("url exactly at the limit: status = %d, want 204", code)
	}
}
