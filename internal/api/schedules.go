package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/AdrianTJ/loadstar/internal/job"
	"github.com/AdrianTJ/loadstar/internal/store"
	"github.com/google/uuid"
)

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		testSpec
		IntervalSeconds int `json:"interval_seconds"`
	}

	if !decodeAndValidate(w, r, &req, &req.testSpec) {
		return
	}

	// Schedule-specific: the recurrence interval.
	if req.IntervalSeconds < job.MinScheduleIntervalS {
		http.Error(w, fmt.Sprintf("interval_seconds must be at least %d", job.MinScheduleIntervalS), http.StatusBadRequest)
		return
	}

	now := time.Now()
	sc := &store.Schedule{
		ID:              "sc_" + uuid.New().String(),
		URL:             req.URL,
		Tiers:           req.Tiers,
		Runs:            req.Runs,
		IntervalSeconds: req.IntervalSeconds,
		Budget:          req.Budget,
		WebhookURL:      req.WebhookURL,
		Profile:         req.Profile,
		Enabled:         true,
		CreatedAt:       now,
		// First run fires on the next scheduler tick — monitoring users
		// expect an immediate first datapoint, not one interval of silence.
		NextRunAt: &now,
	}
	if err := s.store.CreateSchedule(r.Context(), sc); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(sc)
}

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	schedules, err := s.store.ListSchedules(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if schedules == nil {
		schedules = []store.Schedule{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(schedules)
}

func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	sc, err := s.store.GetSchedule(r.Context(), r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sc == nil {
		http.Error(w, "schedule not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

// handleUpdateSchedule supports one mutation: {"enabled": bool}. Everything
// else about a schedule is immutable — delete and recreate instead.
func (s *Server) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req struct {
		Enabled *bool `json:"enabled"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		http.Error(w, `body must be {"enabled": true|false}`, http.StatusBadRequest)
		return
	}

	sc, err := s.store.GetSchedule(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sc == nil {
		http.Error(w, "schedule not found", http.StatusNotFound)
		return
	}

	if err := s.store.SetScheduleEnabled(r.Context(), id, *req.Enabled); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sc.Enabled = *req.Enabled
	writeJSON(w, http.StatusOK, sc)
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sc, err := s.store.GetSchedule(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if sc == nil {
		http.Error(w, "schedule not found", http.StatusNotFound)
		return
	}
	if err := s.store.DeleteSchedule(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
