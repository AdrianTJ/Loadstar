package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AdrianTJ/loadstar/internal/collector/network"
	"github.com/AdrianTJ/loadstar/internal/store"
)

func TestCreateJob_InvalidBudgetRejected(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "api-bad-budget", "", true, nil)

	cases := []string{
		`{"url":"http://example.com","tiers":["network"],"budget":{"assertions":{"bogus.metric":{"max":1}}}}`,
		`{"url":"http://example.com","tiers":["network"],"budget":{"assertions":{"network.ttfb_ms":{}}}}`,
		`{"url":"http://example.com","tiers":["network"],"budget":{"assertions":{"network.ttfb_ms":{"max":1,"level":"critical"}}}}`,
		`{"url":"http://example.com","tiers":["network"],"budget":{"assertions":{}}}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest("POST", "/v1/jobs", bytes.NewReader([]byte(body)))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400 (%s)", body, w.Code, w.Body.String())
		}
	}
}

func TestCreateJob_WithBudgetAcceptedAndEvaluated(t *testing.T) {
	t.Setenv("LOADSTAR_ALLOW_PRIVATE_IPS", "true") // target is a loopback httptest server
	_, mux, _, _ := newTestServer(t, "api-good-budget", "", true, nil)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"url":   target.URL,
		"tiers": []string{"network"},
		"budget": map[string]interface{}{
			"assertions": map[string]interface{}{
				"network.ttfb_ms": map[string]interface{}{"max": 0.001},
			},
		},
	})
	req := httptest.NewRequest("POST", "/v1/jobs", bytes.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", w.Code, w.Body.String())
	}
	var created map[string]string
	json.Unmarshal(w.Body.Bytes(), &created)
	jobID := created["job_id"]

	// Poll the GET endpoint until terminal, then check budget fields.
	var resp map[string]interface{}
	for i := 0; i < 100; i++ {
		req = httptest.NewRequest("GET", "/v1/jobs/"+jobID, nil)
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		json.Unmarshal(w.Body.Bytes(), &resp)
		st, _ := resp["status"].(string)
		if st == "COMPLETED" || st == "PARTIAL" || st == "FAILED" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if _, ok := resp["budget"]; !ok {
		t.Error("GET job response missing budget")
	}
	br, ok := resp["budget_result"].(map[string]interface{})
	if !ok {
		t.Fatalf("GET job response missing budget_result: %v", resp)
	}
	if br["status"] != "fail" {
		t.Errorf("budget_result.status = %v, want fail", br["status"])
	}
}

func TestCreateJob_NoBudgetOmitsBudgetFields(t *testing.T) {
	_, mux, s, _ := newTestServer(t, "api-no-budget", "", true, nil)

	// Seed a completed job directly so GET returns immediately.
	if err := s.CreateJob(context.Background(), &store.Job{
		ID: "jb_plain", URL: "http://example.com", Status: store.StatusCompleted,
		Tiers: []string{"network"}, Runs: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	req := httptest.NewRequest("GET", "/v1/jobs/jb_plain", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if _, ok := resp["budget"]; ok {
		t.Error("budget key present on budget-less job")
	}
	if _, ok := resp["budget_result"]; ok {
		t.Error("budget_result key present on budget-less job")
	}
}

// TestHistory_NewShape seeds results and checks the metrics map with
// percentiles appears in GET /v1/history.
func TestHistory_NewShape(t *testing.T) {
	_, mux, s, _ := newTestServer(t, "api-history", "", true, nil)

	url := "http://example.com/hist"
	s.CreateJob(context.Background(), &store.Job{
		ID: "jb_h", URL: url, Status: store.StatusCompleted,
		Tiers: []string{"network"}, Runs: 2, CreatedAt: time.Now(),
	})
	for i, ttfb := range []float64{100, 300} {
		s.SaveResult(context.Background(), &store.Result{
			ID: "res_h_" + string(rune('a'+i)), JobID: "jb_h", RunIndex: i + 1,
			Network:     &network.Result{TTFBMS: ttfb, TotalMS: ttfb * 2},
			CollectedAt: time.Now(),
		})
	}

	req := httptest.NewRequest("GET", "/v1/history?url="+url, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("history status = %d (%s)", w.Code, w.Body.String())
	}

	var h struct {
		TestCount int `json:"test_count"`
		Metrics   map[string]struct {
			Count int     `json:"count"`
			P75   float64 `json:"p75"`
			Avg   float64 `json:"avg"`
		} `json:"metrics"`
		AvgTTFBMS float64 `json:"avg_ttfb_ms"`
	}
	json.Unmarshal(w.Body.Bytes(), &h)
	if h.TestCount != 2 {
		t.Errorf("test_count = %d, want 2", h.TestCount)
	}
	m, ok := h.Metrics["network.ttfb_ms"]
	if !ok {
		t.Fatalf("metrics missing network.ttfb_ms: %s", w.Body.String())
	}
	if m.Avg != 200 || m.P75 != 250 {
		t.Errorf("ttfb avg/p75 = %v/%v, want 200/250", m.Avg, m.P75)
	}
	if h.AvgTTFBMS != 200 {
		t.Errorf("legacy avg_ttfb_ms = %v, want 200", h.AvgTTFBMS)
	}
}
