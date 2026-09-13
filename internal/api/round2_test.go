package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AdrianTJ/loadstar/internal/job"
	"github.com/AdrianTJ/loadstar/internal/store"
)

// newTestServer builds a server for handler tests: a temp store, a started
// manager, and the given auth settings. RUM origins pass through to
// SetRUMOrigins (nil keeps the ingest endpoint disabled). Returns the server,
// its route handler, the store, and the manager.
func newTestServer(t *testing.T, name, apiKey string, insecure bool, origins []string) (*Server, http.Handler, store.Store, *job.Manager) {
	t.Helper()
	tmpDir, _ := os.MkdirTemp("", name)
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	s, err := store.NewStore(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	m := job.NewManager(s, 1, 10, "")
	m.Start()
	t.Cleanup(m.Stop)

	srv := NewServer(m, s, apiKey, insecure)
	if origins != nil {
		srv.SetRUMOrigins(origins)
	}
	return srv, srv.Routes(), s, m
}

func postJob(t *testing.T, mux http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/jobs", strings.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestCreateJob_InvalidTier(t *testing.T) {
	srv, _, _, _ := newTestServer(t, "api-round2", "", true, nil)
	mux := srv.Routes()

	if w := postJob(t, mux, `{"url":"http://example.com","tiers":["bogus"]}`); w.Code != http.StatusBadRequest {
		t.Errorf("bogus tier: expected 400, got %d", w.Code)
	}
	if w := postJob(t, mux, `{"url":"http://example.com","tiers":["network","all"]}`); w.Code != http.StatusAccepted {
		t.Errorf("valid tiers: expected 202, got %d", w.Code)
	}
	if w := postJob(t, mux, `{"url":"http://example.com","tiers":[]}`); w.Code != http.StatusAccepted {
		t.Errorf("empty tiers: expected 202, got %d", w.Code)
	}
}

func TestDeleteJob_RunningReturns409(t *testing.T) {
	srv, _, s, _ := newTestServer(t, "api-round2", "", true, nil)
	mux := srv.Routes()

	// Seed a RUNNING job directly.
	if err := s.CreateJob(context.Background(), &store.Job{
		ID: "jb_running", URL: "http://x", Status: store.StatusRunning,
		Tiers: []string{"network"}, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := httptest.NewRequest("DELETE", "/v1/jobs/jb_running", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("delete RUNNING job: expected 409, got %d", w.Code)
	}
	// The row must survive the rejected delete.
	if j, _ := s.GetJob(context.Background(), "jb_running"); j == nil {
		t.Error("running job should not have been deleted")
	}
}

func TestCreateJob_TimeoutBounds(t *testing.T) {
	srv, _, s, _ := newTestServer(t, "api-round2", "", true, nil)
	mux := srv.Routes()

	if w := postJob(t, mux, `{"url":"http://example.com","tiers":["network"],"timeout_s":700}`); w.Code != http.StatusBadRequest {
		t.Errorf("timeout_s 700: expected 400, got %d", w.Code)
	}
	w := postJob(t, mux, `{"url":"http://example.com","tiers":["network"],"timeout_s":30}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("timeout_s 30: expected 202, got %d", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	j, _ := s.GetJob(context.Background(), resp["job_id"])
	if j == nil || j.TimeoutS != 30 {
		t.Errorf("expected stored timeout_s=30, got %+v", j)
	}
}

func TestCreateJob_BodyTooLarge(t *testing.T) {
	srv, _, _, _ := newTestServer(t, "api-round2", "", true, nil)
	mux := srv.Routes()

	big := `{"url":"http://example.com","webhook_url":"` + strings.Repeat("a", 2<<20) + `"}`
	req := httptest.NewRequest("POST", "/v1/jobs", bytes.NewReader([]byte(big)))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("oversize body: expected 400, got %d", w.Code)
	}
}
