package lighthouse

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const secretKey = "AIzaSy-SECRET-KEY-MUST-NOT-APPEAR"

// TestCollect_TransportErrorRedactsAPIKey is the regression for the July 2026
// audit finding SEC-1. The PSI key rides in the query string, and Go's
// transport errors quote the whole request URL, so an unredacted error path
// carried the key into jobs.error and out through GET /v1/jobs/{id}.
func TestCollect_TransportErrorRedactsAPIKey(t *testing.T) {
	// A server that is closed immediately, so Collect hits a connection error
	// rather than an HTTP status error — that is the path that embeds the URL.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := server.URL
	server.Close()

	oldEndpoint := currentEndpoint()
	SetEndpoint(dead)
	defer SetEndpoint(oldEndpoint)

	_, err := Collect(context.Background(), "https://example.com", secretKey)
	if err == nil {
		t.Fatal("Collect() error = nil, want a transport error")
	}
	if strings.Contains(err.Error(), secretKey) {
		t.Errorf("error leaks the PSI API key:\n  %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Errorf("error should mark the query as redacted, got:\n  %v", err)
	}
}

// The redaction must survive whatever wrapping the caller applies, since the
// job manager formats the error into a status string before persisting it.
func TestRedactURLError(t *testing.T) {
	t.Run("passes through non-url errors", func(t *testing.T) {
		in := context.DeadlineExceeded
		if got := redactURLError(in); got != in {
			t.Errorf("redactURLError(%v) = %v, want the original error", in, got)
		}
	})

	t.Run("strips query and credentials", func(t *testing.T) {
		in := &url.Error{
			Op:  "Get",
			URL: "https://user:pw@www.googleapis.com/v5/runPagespeed?key=" + secretKey,
			Err: errors.New("connection refused"),
		}
		got := redactURLError(in).Error()
		for _, leak := range []string{secretKey, "pw@", "key="} {
			if strings.Contains(got, leak) {
				t.Errorf("redacted error still contains %q:\n  %s", leak, got)
			}
		}
		// The useful parts must survive — a redacted error still has to be
		// debuggable.
		for _, keep := range []string{"Get", "www.googleapis.com", "connection refused"} {
			if !strings.Contains(got, keep) {
				t.Errorf("redacted error lost %q:\n  %s", keep, got)
			}
		}
	})
}

func TestCollect(t *testing.T) {
	// Mock PSI API response
	mockResponse := struct {
		LighthouseResult struct {
			LighthouseVersion string `json:"lighthouseVersion"`
			FetchTime         string `json:"fetchTime"`
			Categories        map[string]struct {
				Score float64 `json:"score"`
			} `json:"categories"`
		} `json:"lighthouseResult"`
	}{
		LighthouseResult: struct {
			LighthouseVersion string `json:"lighthouseVersion"`
			FetchTime         string `json:"fetchTime"`
			Categories        map[string]struct {
				Score float64 `json:"score"`
			} `json:"categories"`
		}{
			LighthouseVersion: "11.0.0",
			FetchTime:         "2026-04-27T10:00:00Z",
			Categories: map[string]struct {
				Score float64 `json:"score"`
			}{
				"performance":    {Score: 0.95},
				"accessibility":  {Score: 0.90},
				"best-practices": {Score: 0.85},
				"seo":            {Score: 0.80},
				"pwa":            {Score: 0.75},
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(mockResponse)
	}))
	defer server.Close()

	// Override endpoint
	oldEndpoint := currentEndpoint()
	SetEndpoint(server.URL)
	defer SetEndpoint(oldEndpoint)

	res, err := Collect(context.Background(), "https://example.com", "test-api-key")
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}

	if res.Performance != 0.95 {
		t.Errorf("expected performance 0.95, got %.2f", res.Performance)
	}
	if res.LighthouseVer != "11.0.0" {
		t.Errorf("expected version 11.0.0, got %s", res.LighthouseVer)
	}
	if res.PWA != 0.75 {
		t.Errorf("expected PWA 0.75, got %.2f", res.PWA)
	}
}
