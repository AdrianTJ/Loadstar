package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIServer_FailSecureAuth(t *testing.T) {
	apiKey := "top-secret"
	_, mux, _, _ := newTestServer(t, "auth-test", apiKey, false, nil)

	routes := []struct {
		method string
		path   string
	}{
		{"GET", "/v1/jobs"},
		{"POST", "/v1/jobs"},
		{"GET", "/v1/history?url=test"},
		{"GET", "/v1/health"},
		{"GET", "/v1/ready"},
	}

	for _, rt := range routes {
		t.Run(rt.path, func(t *testing.T) {
			req := httptest.NewRequest(rt.method, rt.path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if rt.path == "/v1/health" || rt.path == "/v1/ready" {
				if w.Code != http.StatusOK {
					t.Errorf("%s %s without key: expected 200, got %d", rt.method, rt.path, w.Code)
				}
				return
			}

			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s without key: expected 401, got %d", rt.method, rt.path, w.Code)
			}

			req = httptest.NewRequest(rt.method, rt.path, nil)
			req.Header.Set("X-API-Key", "wrong-password")
			w = httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with wrong key: expected 401, got %d", rt.method, rt.path, w.Code)
			}

			req = httptest.NewRequest(rt.method, rt.path, nil)
			req.Header.Set("X-API-Key", apiKey)
			w = httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code == http.StatusUnauthorized {
				t.Errorf("%s %s with correct key: expected success, got 401", rt.method, rt.path)
			}
		})
	}
}

func TestAPIServer_MisconfiguredAuth(t *testing.T) {
	// No key AND no insecure flag
	_, mux, _, _ := newTestServer(t, "misconfig-auth", "", false, nil)

	req := httptest.NewRequest("GET", "/v1/jobs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for misconfigured auth, got %d", w.Code)
	}
}

func TestAPIServer_ConstantTimeAuthBoundaries(t *testing.T) {
	apiKey := "my-secret-key"
	_, mux, _, _ := newTestServer(t, "ct-auth-bounds", apiKey, false, nil)

	// 1. Test empty key header (X-API-Key is "")
	req := httptest.NewRequest("GET", "/v1/jobs", nil)
	req.Header.Set("X-API-Key", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("empty X-API-Key: expected 401, got %d", w.Code)
	}

	// 2. Test extremely long key header to verify it doesn't crash subtle.ConstantTimeCompare
	longKey := make([]byte, 10000)
	for i := range longKey {
		longKey[i] = 'a'
	}
	req = httptest.NewRequest("GET", "/v1/jobs", nil)
	req.Header.Set("X-API-Key", string(longKey))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("extremely long X-API-Key: expected 401, got %d", w.Code)
	}
}
