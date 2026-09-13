package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/AdrianTJ/loadstar/docs"
	"github.com/AdrianTJ/loadstar/internal/budget"
	"github.com/AdrianTJ/loadstar/internal/job"
	"github.com/AdrianTJ/loadstar/internal/profile"
	"github.com/AdrianTJ/loadstar/internal/store"
	"github.com/AdrianTJ/loadstar/internal/tier"
	"github.com/AdrianTJ/loadstar/internal/validator"
)

const (
	maxRequestBody      = 1 << 20 // 1 MiB cap on request bodies
	maxTimeoutS         = 600     // upper bound on a caller-supplied per-job timeout
	maxRuns             = 10      // upper bound on the runs parameter
	defaultJobListLimit = 50      // number of jobs GET /v1/jobs returns
)

// validateTiers rejects unknown tier names so a typo does not silently produce
// a COMPLETED job with no results. An empty list is allowed (defaults to network).
func validateTiers(tiers []string) error {
	for _, t := range tiers {
		if !tier.Valid(t) {
			return fmt.Errorf("invalid tier %q (allowed: %s)", t, strings.Join(tier.Supported, ", "))
		}
	}
	return nil
}

// testSpec is the "what to measure and how" shared by job and schedule
// creation. Both endpoints accept the same fields with the same rules, and
// keeping the checks in one place is a security property rather than a tidiness
// one: when these were duplicated, the July 2026 SSRF allow-list gap had to be
// fixed and tested on two separate paths, and a fix applied to one and missed
// on the other would have left the hole open on the other endpoint.
//
// Embedded anonymously into each request struct, so callers still send (and the
// OpenAPI spec still documents) one flat JSON object.
type testSpec struct {
	URL        string         `json:"url"`
	Tiers      []string       `json:"tiers"`
	Runs       int            `json:"runs"`
	WebhookURL string         `json:"webhook_url"`
	Budget     *budget.Budget `json:"budget"`
	Profile    string         `json:"profile"`
}

// validate checks the shared fields and normalises Runs in place. The returned
// error is written straight to the client, so its text is part of the API's
// contract; the messages match what each endpoint returned previously.
func (spec *testSpec) validate() error {
	if spec.URL == "" {
		return errors.New("url is required")
	}
	if err := validator.ValidateURL(spec.URL); err != nil {
		return err
	}
	if err := validateTiers(spec.Tiers); err != nil {
		return err
	}
	if spec.WebhookURL != "" {
		if err := validator.ValidateURL(spec.WebhookURL); err != nil {
			return fmt.Errorf("invalid webhook_url: %w", err)
		}
	}
	if spec.Runs <= 0 {
		spec.Runs = 1
	}
	if spec.Runs > maxRuns {
		return fmt.Errorf("runs parameter cannot exceed %d", maxRuns)
	}
	if spec.Budget != nil {
		if err := spec.Budget.Validate(); err != nil {
			return fmt.Errorf("invalid budget: %w", err)
		}
	}
	if !profile.Valid(spec.Profile) {
		return profile.ErrUnknown(spec.Profile)
	}
	return nil
}

// writeJSON sets the JSON content type and encodes v with the given status.
// It mirrors what every handler used to do inline (header, optional status,
// encode, ignoring the encode error) in one call.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// decodeAndValidate reads a size-capped JSON body into dst and runs the shared
// validation. It writes the 400 response itself and reports whether the handler
// should continue.
func decodeAndValidate(w http.ResponseWriter, r *http.Request, dst any, spec *testSpec) bool {
	// Cap the request body so a large payload cannot exhaust memory.
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return false
	}
	if err := spec.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

type Server struct {
	manager       *job.Manager
	store         store.Store
	apiKey        string
	allowInsecure bool
	rumOrigins    []string // allowed Origins for POST /v1/rum; empty = endpoint disabled
	rumLimiter    *tokenBucket
}

func NewServer(m *job.Manager, s store.Store, apiKey string, allowInsecure bool) *Server {
	return &Server{
		manager:       m,
		store:         s,
		apiKey:        apiKey,
		allowInsecure: allowInsecure,
		rumLimiter:    newTokenBucket(rumEventsPerSecond, rumBurst),
	}
}

// SetRUMOrigins enables the public RUM ingest endpoint for the given origins
// (comma-separated list already split by the caller; "*" allows any origin).
// With no origins configured the endpoint stays disabled (404).
func (s *Server) SetRUMOrigins(origins []string) {
	s.rumOrigins = origins
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey == "" {
			if s.allowInsecure {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, "server misconfigured: LOADSTAR_API_KEY is required for this route (or set LOADSTAR_ALLOW_INSECURE=true for local testing)", http.StatusInternalServerError)
			return
		}

		key := r.Header.Get("X-API-Key")
		if subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders applies the headers that should be on every response. The
// CSP here is the restrictive default for JSON APIs — routes that serve HTML
// (/ and /docs) set their own, and a header already set by the handler is left
// alone.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if h.Get("Content-Security-Policy") == "" {
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		}
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// Public routes
	mux.HandleFunc("GET /{$}", s.handleUI) // status page; exact "/" only
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/ready", s.handleReady)
	mux.HandleFunc("GET /openapi.yaml", s.handleOpenAPI)
	mux.HandleFunc("GET /docs", s.handleDocs)

	// Protected routes
	mux.Handle("GET /v1/history", s.authMiddleware(http.HandlerFunc(s.handleHistory)))
	mux.Handle("POST /v1/jobs", s.authMiddleware(http.HandlerFunc(s.handleCreateJob)))
	mux.Handle("GET /v1/jobs", s.authMiddleware(http.HandlerFunc(s.handleListJobs)))
	mux.Handle("GET /v1/jobs/{id}", s.authMiddleware(http.HandlerFunc(s.handleGetJob)))
	mux.Handle("DELETE /v1/jobs/{id}", s.authMiddleware(http.HandlerFunc(s.handleDeleteJob)))

	// Schedules (recurring monitors)
	mux.Handle("POST /v1/schedules", s.authMiddleware(http.HandlerFunc(s.handleCreateSchedule)))
	mux.Handle("GET /v1/schedules", s.authMiddleware(http.HandlerFunc(s.handleListSchedules)))
	mux.Handle("GET /v1/schedules/{id}", s.authMiddleware(http.HandlerFunc(s.handleGetSchedule)))
	mux.Handle("PATCH /v1/schedules/{id}", s.authMiddleware(http.HandlerFunc(s.handleUpdateSchedule)))
	mux.Handle("DELETE /v1/schedules/{id}", s.authMiddleware(http.HandlerFunc(s.handleDeleteSchedule)))

	// Prometheus metrics. Auth-protected: an open endpoint would leak the set
	// of tested URLs. Prometheus scrape configs can send the X-API-Key header.
	mux.Handle("GET /metrics", s.authMiddleware(http.HandlerFunc(s.handleMetrics)))

	// RUM ingest: public by design (browsers beacon here), but disabled until
	// LOADSTAR_RUM_ORIGINS is configured; the summary endpoint stays protected.
	mux.HandleFunc("POST /v1/rum", s.handleRUMIngest)
	mux.HandleFunc("OPTIONS /v1/rum", s.handleRUMPreflight)
	mux.Handle("GET /v1/rum/summary", s.authMiddleware(http.HandlerFunc(s.handleRUMSummary)))

	return securityHeaders(mux)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := s.manager.Metrics().Write(w); err != nil {
		slog.Error("Failed to write metrics", "error", err)
	}
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	// Served from the embedded copy so /docs works regardless of the
	// daemon's working directory.
	w.Header().Set("Content-Type", "application/yaml")
	w.Write(docs.OpenAPISpec)
}

// Subresource Integrity hashes for the pinned Swagger UI assets. Without
// these, /docs — a public route — executes whatever unpkg.com happens to
// serve, on the same origin where the status page keeps the operator's API key
// in localStorage. A compromised or hijacked CDN response could read it.
//
// Both hashes are sha384 over the files in swagger-ui-dist@5.11.0. Recompute
// them whenever the pinned version changes:
//
//	curl -sL https://unpkg.com/swagger-ui-dist@<ver>/<file> |
//	  openssl dgst -sha384 -binary | openssl base64 -A
const (
	swaggerVersion = "5.11.0"
	swaggerCSSHash = "sha384-+yyzNgM3K92sROwsXxYCxaiLWxWJ0G+v/9A+qIZ2rgefKgkdcmJI+L601cqPD/Ut"
	swaggerJSHash  = "sha384-qn5tagrAjZi8cSmvZ+k3zk4+eDEEUcP9myuR2J6V+/H6rne++v6ChO7EeHAEzqxQ"
	// sha384 of the inline bootstrap <script> body below, so the CSP can pin it
	// by hash instead of opening the page up with 'unsafe-inline'. A test
	// recomputes this from the rendered page, so editing the script without
	// updating the hash fails the suite rather than breaking /docs at runtime.
	swaggerBootstrap = "sha384-/hWp1J+9Jn6vvz6oTt5TLHx8hgY5M2aTGcksS8IiOacuhk8rmvWuooUYu9NPKkrP"
)

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Scoped CSP: /docs is the one route that loads third-party assets, so it
	// gets the unpkg exception the rest of the daemon does not. The inline
	// bootstrap below is pinned by hash rather than allowed via unsafe-inline.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; "+
			"script-src https://unpkg.com '"+swaggerBootstrap+"'; "+
			"style-src https://unpkg.com 'unsafe-inline'; "+
			"img-src 'self' data:; "+
			"connect-src 'self'; "+
			"base-uri 'none'; frame-ancestors 'none'")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Loadstar API Docs</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@%[1]s/swagger-ui.css"
        integrity="%[2]s" crossorigin="anonymous" />
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@%[1]s/swagger-ui-bundle.js"
          integrity="%[3]s" crossorigin="anonymous"></script>
  <script>window.onload=()=>{window.ui=SwaggerUIBundle({url:'/openapi.yaml',dom_id:'#swagger-ui'})}</script>
</body>
</html>
`, swaggerVersion, swaggerCSSHash, swaggerJSHash)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.ListJobs(r.Context(), 1); err != nil {
		http.Error(w, "database not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("READY"))
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req struct {
		testSpec
		TimeoutS int `json:"timeout_s"`
	}

	if !decodeAndValidate(w, r, &req, &req.testSpec) {
		return
	}

	// Job-specific: the per-run timeout has no meaning for a schedule, which
	// inherits the daemon default.
	if req.TimeoutS < 0 || req.TimeoutS > maxTimeoutS {
		http.Error(w, fmt.Sprintf("timeout_s must be between 0 and %d", maxTimeoutS), http.StatusBadRequest)
		return
	}

	created, err := s.manager.SubmitJob(r.Context(), job.SubmitOptions{
		URL:        req.URL,
		Tiers:      req.Tiers,
		Runs:       req.Runs,
		TimeoutS:   req.TimeoutS,
		WebhookURL: req.WebhookURL,
		Budget:     req.Budget,
		Profile:    req.Profile,
	})

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{
		"job_id": created.ID,
		"status": string(created.Status),
	})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing job id", http.StatusBadRequest)
		return
	}

	job, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if job == nil {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}

	results, err := s.store.GetResultsByJobID(r.Context(), id)
	if err != nil {
		slog.Error("Failed to fetch job results", "job_id", id, "error", err)
	}

	resp := map[string]any{
		"job_id":       job.ID,
		"status":       job.Status,
		"url":          job.URL,
		"created_at":   job.CreatedAt,
		"completed_at": job.CompletedAt,
		"error":        job.Error,
		"results":      results,
	}
	if job.Budget != nil {
		resp["budget"] = job.Budget
	}
	if job.BudgetResult != nil {
		resp["budget_result"] = job.BudgetResult
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.ListJobs(r.Context(), defaultJobListLimit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	url := r.URL.Query().Get("url")
	if url == "" {
		http.Error(w, "missing url parameter", http.StatusBadRequest)
		return
	}

	history, err := s.store.GetHistory(r.Context(), url)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, history)
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing job id", http.StatusBadRequest)
		return
	}

	// Refuse to delete a job that is actively running: deleting its row would
	// make the worker's in-flight SaveResult fail a foreign-key constraint and
	// silently drop the run's data.
	if job, err := s.store.GetJob(r.Context(), id); err == nil && job != nil && job.Status == store.StatusRunning {
		http.Error(w, "job is running; cannot delete until it finishes", http.StatusConflict)
		return
	}

	// Cancel it first if it's still pending.
	_ = s.manager.CancelJob(r.Context(), id)

	if err := s.store.DeleteJob(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
