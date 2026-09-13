package api

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/AdrianTJ/loadstar/internal/store"
)

func TestAPIServer(t *testing.T) {
	// Setup — insecure mode for the base tests.
	_, mux, s, m := newTestServer(t, "api-server-test", "", true, nil)

	// 1. Test POST /v1/jobs
	reqBody := map[string]interface{}{
		"url":   "http://example.com",
		"runs":  1,
		"tiers": []string{"network"},
	}
	body, _ := json.Marshal(reqBody)
	req := httptest.NewRequest("POST", "/v1/jobs", bytes.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected status 202, got %d", w.Code)
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	jobID := resp["job_id"].(string)

	if jobID == "" {
		t.Fatal("expected job_id in response")
	}

	// 2. Test GET /v1/jobs/{id}
	// Wait a bit for the worker to pick it up (though we don't strictly need it to finish for GET to work)
	time.Sleep(100 * time.Millisecond)

	req = httptest.NewRequest("GET", "/v1/jobs/"+jobID, nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var getResp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &getResp)
	if getResp["job_id"] != jobID {
		t.Errorf("expected job_id %s, got %s", jobID, getResp["job_id"])
	}
	if getResp["url"] != "http://example.com" {
		t.Errorf("expected url http://example.com, got %s", getResp["url"])
	}

	// 3. Test Authentication (with an API key)
	srvWithAuth := NewServer(m, s, "secret-key", false)
	muxWithAuth := srvWithAuth.Routes()

	// 3a. Unauthorized request
	req = httptest.NewRequest("GET", "/v1/jobs/"+jobID, nil)
	w = httptest.NewRecorder()
	muxWithAuth.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got %d", w.Code)
	}

	// 3b. Authorized request
	req = httptest.NewRequest("GET", "/v1/jobs/"+jobID, nil)
	req.Header.Set("X-API-Key", "secret-key")
	w = httptest.NewRecorder()
	muxWithAuth.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 OK with valid API key, got %d", w.Code)
	}

	// 4. Test Health & Ready
	req = httptest.NewRequest("GET", "/v1/health", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected health 200, got %d", w.Code)
	}

	req = httptest.NewRequest("GET", "/v1/ready", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected ready 200, got %d", w.Code)
	}

	// 5. Test List Jobs
	req = httptest.NewRequest("GET", "/v1/jobs", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected list jobs 200, got %d", w.Code)
	}
	var jobs []store.Job
	if err := json.Unmarshal(w.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("failed to unmarshal jobs list: %v", err)
	}
	if len(jobs) < 1 {
		t.Error("expected at least 1 job in list")
	}

	// 6. Test Edge Cases
	// 6a. POST without URL
	req = httptest.NewRequest("POST", "/v1/jobs", bytes.NewReader([]byte(`{"runs": 1}`)))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for missing URL, got %d", w.Code)
	}

	// 6b. GET History for unknown URL
	req = httptest.NewRequest("GET", "/v1/history?url=https://not-exists.com", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 OK for empty history, got %d", w.Code)
	}
	var history map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &history)
	if history["test_count"].(float64) != 0 {
		t.Errorf("expected 0 tests in history, got %v", history["test_count"])
	}
}

func TestAPIServer_WebhookSSRF(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "webhook-ssrf-test", "", true, nil)

	tests := []struct {
		webhookURL string
		wantCode   int
	}{
		{"http://127.0.0.1", http.StatusBadRequest},
		{"http://localhost", http.StatusBadRequest},
		{"http://169.254.169.254", http.StatusBadRequest},
		{"http://10.0.0.1", http.StatusBadRequest},
		{"https://example.com/webhook", http.StatusAccepted},
		{"", http.StatusAccepted},
	}

	for _, tt := range tests {
		t.Run(tt.webhookURL, func(t *testing.T) {
			reqBody := map[string]interface{}{
				"url":         "http://example.com",
				"runs":        1,
				"webhook_url": tt.webhookURL,
			}
			body, _ := json.Marshal(reqBody)
			req := httptest.NewRequest("POST", "/v1/jobs", bytes.NewReader(body))
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != tt.wantCode {
				t.Errorf("WebhookURL %q: expected code %d, got %d", tt.webhookURL, tt.wantCode, w.Code)
			}
		})
	}
}

func TestAPIServer_RunsConstraint(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "runs-constraint-test", "", true, nil)

	tests := []struct {
		runs     int
		wantCode int
	}{
		{0, http.StatusAccepted},
		{1, http.StatusAccepted},
		{10, http.StatusAccepted},
		{11, http.StatusBadRequest},
		{100, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("runs-%d", tt.runs), func(t *testing.T) {
			reqBody := map[string]interface{}{
				"url":  "http://example.com",
				"runs": tt.runs,
			}
			body, _ := json.Marshal(reqBody)
			req := httptest.NewRequest("POST", "/v1/jobs", bytes.NewReader(body))
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != tt.wantCode {
				t.Errorf("Runs %d: expected code %d, got %d", tt.runs, tt.wantCode, w.Code)
			}
		})
	}
}

// TestDocs_SubresourceIntegrity is part of the regression for the July 2026
// audit finding SEC-4. /docs is public and pulls Swagger UI from unpkg.com
// onto the same origin where the status page stores the API key, so both
// assets must be pinned by hash.
func TestDocs_SubresourceIntegrity(t *testing.T) {
	srv := &Server{}
	rec := httptest.NewRecorder()
	srv.handleDocs(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))
	body := rec.Body.String()

	if n := strings.Count(body, `integrity="sha384-`); n != 2 {
		t.Errorf("found %d SRI hashes in /docs, want 2 (css + js)", n)
	}
	if n := strings.Count(body, `crossorigin="anonymous"`); n != 2 {
		t.Errorf("found %d crossorigin attributes, want 2 — SRI is not enforced without it", n)
	}
	for _, want := range []string{swaggerCSSHash, swaggerJSHash} {
		if !strings.Contains(body, want) {
			t.Errorf("/docs is missing hash %s", want)
		}
	}
	if !strings.Contains(body, "unpkg.com/swagger-ui-dist@"+swaggerVersion+"/") {
		t.Errorf("/docs does not pin swagger-ui-dist@%s", swaggerVersion)
	}
}

// The CSP pins the page's inline bootstrap by hash. If the script body is
// edited without recomputing swaggerBootstrap the browser silently refuses to
// run it, so the mismatch is caught here instead.
func TestDocs_InlineScriptHashMatchesCSP(t *testing.T) {
	srv := &Server{}
	rec := httptest.NewRecorder()
	srv.handleDocs(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))

	m := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatal("no inline <script> found in /docs")
	}
	sum := sha512.Sum384([]byte(m[1]))
	got := "sha384-" + base64.StdEncoding.EncodeToString(sum[:])

	if got != swaggerBootstrap {
		t.Errorf("inline script hash drifted.\n  csp:      %s\n  computed: %s\n"+
			"update swaggerBootstrap to the computed value", swaggerBootstrap, got)
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), swaggerBootstrap) {
		t.Error("/docs CSP does not carry the inline-script hash")
	}
}

// TestSecurityHeaders covers the headers that must be on every response, and
// the rule that HTML routes may override the default CSP.
func TestSecurityHeaders(t *testing.T) {
	t.Run("defaults applied to API responses", func(t *testing.T) {
		h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/jobs", nil))

		for header, want := range map[string]string{
			"Content-Security-Policy": "default-src 'none'; frame-ancestors 'none'; base-uri 'none'",
			"X-Content-Type-Options":  "nosniff",
			"X-Frame-Options":         "DENY",
			"Referrer-Policy":         "no-referrer",
		} {
			if got := rec.Header().Get(header); got != want {
				t.Errorf("%s = %q, want %q", header, got, want)
			}
		}
	})

	t.Run("handler CSP wins", func(t *testing.T) {
		const own = "default-src 'self'"
		h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy", own)
		}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

		if got := rec.Header().Get("Content-Security-Policy"); got != own {
			t.Errorf("CSP = %q, want the handler's own %q", got, own)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Error("non-CSP headers should still be applied")
		}
	})
}
