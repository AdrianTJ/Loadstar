package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AdrianTJ/loadstar/internal/store"
)

func doJSON(t *testing.T, mux http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != "" {
		rdr = bytes.NewReader([]byte(body))
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestScheduleLifecycle(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "api-sched-crud", "", true, nil)

	// Create.
	w := doJSON(t, mux, "POST", "/v1/schedules",
		`{"url":"http://example.com","tiers":["network"],"runs":2,"interval_seconds":120,
		  "budget":{"assertions":{"network.ttfb_ms":{"max":500}}}}`, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d (%s)", w.Code, w.Body.String())
	}
	var sc store.Schedule
	json.Unmarshal(w.Body.Bytes(), &sc)
	if sc.ID == "" || !strings.HasPrefix(sc.ID, "sc_") {
		t.Fatalf("bad schedule id: %q", sc.ID)
	}
	if !sc.Enabled || sc.NextRunAt == nil {
		t.Errorf("new schedule must be enabled with next_run_at set: %+v", sc)
	}

	// List.
	w = doJSON(t, mux, "GET", "/v1/schedules", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), sc.ID) {
		t.Errorf("list = %d %s", w.Code, w.Body.String())
	}

	// Get.
	w = doJSON(t, mux, "GET", "/v1/schedules/"+sc.ID, "", nil)
	if w.Code != http.StatusOK {
		t.Errorf("get = %d", w.Code)
	}

	// Disable via PATCH.
	w = doJSON(t, mux, "PATCH", "/v1/schedules/"+sc.ID, `{"enabled":false}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("patch = %d (%s)", w.Code, w.Body.String())
	}
	var patched store.Schedule
	json.Unmarshal(w.Body.Bytes(), &patched)
	if patched.Enabled {
		t.Error("schedule still enabled after PATCH")
	}

	// Delete.
	w = doJSON(t, mux, "DELETE", "/v1/schedules/"+sc.ID, "", nil)
	if w.Code != http.StatusNoContent {
		t.Errorf("delete = %d", w.Code)
	}
	w = doJSON(t, mux, "GET", "/v1/schedules/"+sc.ID, "", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", w.Code)
	}
}

func TestCreateSchedule_Validation(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "api-sched-valid", "", true, nil)

	cases := []struct {
		name string
		body string
	}{
		{"missing url", `{"tiers":["network"],"interval_seconds":60}`},
		{"bad tier", `{"url":"http://example.com","tiers":["bogus"],"interval_seconds":60}`},
		{"interval too small", `{"url":"http://example.com","tiers":["network"],"interval_seconds":30}`},
		{"missing interval", `{"url":"http://example.com","tiers":["network"]}`},
		{"too many runs", `{"url":"http://example.com","tiers":["network"],"interval_seconds":60,"runs":99}`},
		{"invalid budget", `{"url":"http://example.com","tiers":["network"],"interval_seconds":60,"budget":{"assertions":{"nope":{"max":1}}}}`},
		{"private-ip url", `{"url":"http://127.0.0.1/","tiers":["network"],"interval_seconds":60}`},
	}
	for _, c := range cases {
		w := doJSON(t, mux, "POST", "/v1/schedules", c.body, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%s)", c.name, w.Code, w.Body.String())
		}
	}

	// Bad PATCH bodies.
	w := doJSON(t, mux, "POST", "/v1/schedules",
		`{"url":"http://example.com","tiers":["network"],"interval_seconds":60}`, nil)
	var sc store.Schedule
	json.Unmarshal(w.Body.Bytes(), &sc)
	for _, body := range []string{`{}`, `{"url":"http://other.example"}`, `not json`} {
		w = doJSON(t, mux, "PATCH", "/v1/schedules/"+sc.ID, body, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("patch %q: status = %d, want 400", body, w.Code)
		}
	}
}

func TestSchedulesRequireAuth(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "api-sched-auth", "sekrit", false, nil)

	paths := []struct{ method, path string }{
		{"POST", "/v1/schedules"},
		{"GET", "/v1/schedules"},
		{"GET", "/v1/schedules/sc_x"},
		{"PATCH", "/v1/schedules/sc_x"},
		{"DELETE", "/v1/schedules/sc_x"},
		{"GET", "/metrics"},
	}
	for _, p := range paths {
		w := doJSON(t, mux, p.method, p.path, "", nil)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without key = %d, want 401", p.method, p.path, w.Code)
		}
	}
}

func TestMetricsEndpoint(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "api-metrics", "sekrit", false, nil)

	w := doJSON(t, mux, "GET", "/metrics", "", map[string]string{"X-API-Key": "sekrit"})
	if w.Code != http.StatusOK {
		t.Fatalf("metrics = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	body := w.Body.String()
	for _, want := range []string{"# TYPE loadstar_queue_depth gauge", "loadstar_queue_depth 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q:\n%s", want, body)
		}
	}
}

// TestSharedSpecValidation_JobsAndSchedulesAgree pins the property that made
// the refactor worth doing: /v1/jobs and /v1/schedules accept the same "what to
// measure" fields, so they must reject the same input with the same message.
//
// When these checks were duplicated, the July 2026 SSRF allow-list gap had to
// be fixed and tested on both paths independently — a fix landing on one and
// missing the other would have left the hole open on the other endpoint. This
// test fails if the two ever drift apart again.
func TestSharedSpecValidation_JobsAndSchedulesAgree(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "shared-spec", "", true, nil)

	// Each case supplies only the shared fields; the endpoint-specific ones
	// (timeout_s, interval_seconds) are added per request below and are valid
	// throughout, so any 400 must come from the shared validation.
	cases := []struct {
		name string
		spec string
	}{
		{"missing url", `"url":""`},
		{"private ip target", `"url":"http://127.0.0.1/"`},
		{"cgnat target", `"url":"http://100.100.100.200/"`},
		{"metadata target", `"url":"http://169.254.169.254/"`},
		{"bad scheme", `"url":"ftp://example.com/"`},
		{"unknown tier", `"url":"http://93.184.216.34/","tiers":["bogus"]`},
		{"runs over cap", `"url":"http://93.184.216.34/","runs":11`},
		{"unknown profile", `"url":"http://93.184.216.34/","profile":"warp-9"`},
		{"invalid budget", `"url":"http://93.184.216.34/","budget":{"assertions":{"nope.metric":{"max":1}}}`},
		{"webhook to private ip", `"url":"http://93.184.216.34/","webhook_url":"http://10.0.0.1/hook"`},
		{"webhook to cgnat", `"url":"http://93.184.216.34/","webhook_url":"http://100.64.0.1/hook"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobResp := doJSON(t, mux, http.MethodPost, "/v1/jobs",
				"{"+tc.spec+`,"timeout_s":30}`, nil)
			schedResp := doJSON(t, mux, http.MethodPost, "/v1/schedules",
				"{"+tc.spec+`,"interval_seconds":300}`, nil)

			if jobResp.Code != http.StatusBadRequest {
				t.Errorf("/v1/jobs status = %d, want 400 (body: %s)",
					jobResp.Code, strings.TrimSpace(jobResp.Body.String()))
			}
			if schedResp.Code != http.StatusBadRequest {
				t.Errorf("/v1/schedules status = %d, want 400 (body: %s)",
					schedResp.Code, strings.TrimSpace(schedResp.Body.String()))
			}
			if got, want := strings.TrimSpace(schedResp.Body.String()), strings.TrimSpace(jobResp.Body.String()); got != want {
				t.Errorf("endpoints disagree on the rejection reason:\n  /v1/jobs:      %q\n  /v1/schedules: %q", want, got)
			}
		})
	}
}

// The shared validation must not swallow the endpoint-specific rules.
func TestEndpointSpecificValidationStillApplies(t *testing.T) {
	_, mux, _, _ := newTestServer(t, "spec-specific", "", true, nil)
	const okURL = `"url":"http://93.184.216.34/"`

	if w := doJSON(t, mux, http.MethodPost, "/v1/jobs", "{"+okURL+`,"timeout_s":9999}`, nil); w.Code != http.StatusBadRequest {
		t.Errorf("out-of-range timeout_s: status = %d, want 400", w.Code)
	}
	if w := doJSON(t, mux, http.MethodPost, "/v1/schedules", "{"+okURL+`,"interval_seconds":5}`, nil); w.Code != http.StatusBadRequest {
		t.Errorf("below-minimum interval_seconds: status = %d, want 400", w.Code)
	}
	// A schedule must not inherit the job-only timeout_s rule, nor vice versa.
	if w := doJSON(t, mux, http.MethodPost, "/v1/schedules", "{"+okURL+`,"interval_seconds":300,"timeout_s":9999}`, nil); w.Code != http.StatusCreated {
		t.Errorf("schedule with an irrelevant timeout_s: status = %d, want 201 (body: %s)",
			w.Code, strings.TrimSpace(w.Body.String()))
	}
}
