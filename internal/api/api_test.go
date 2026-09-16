package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBearerAuthentication(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	called := false
	handler := securityHeaders((&API{token: token}).auth(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, authorization := range []string{"", "Bearer wrong", "Basic " + token} {
		called = false
		r := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
		r.Header.Set("Authorization", authorization)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || called {
			t.Fatalf("authorization %q returned %d, called=%v", authorization, w.Code, called)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q", got)
		}
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || !called {
		t.Fatalf("valid authorization returned %d, called=%v", w.Code, called)
	}
}

func TestStrictJSON(t *testing.T) {
	type request struct {
		Reference string `json:"reference"`
	}
	tests := []struct {
		name        string
		contentType string
		body        string
		wantOK      bool
		wantStatus  int
	}{
		{"valid", "application/json; charset=utf-8", `{"reference":"customer-1"}`, true, http.StatusOK},
		{"wrong content type", "text/plain", `{}`, false, http.StatusUnsupportedMediaType},
		{"lookalike content type", "application/jsonthing", `{}`, false, http.StatusUnsupportedMediaType},
		{"unknown field", "application/json", `{"reference":"x","extra":true}`, false, http.StatusBadRequest},
		{"trailing value", "application/json", `{"reference":"x"} {}`, false, http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body))
			r.Header.Set("Content-Type", test.contentType)
			w := httptest.NewRecorder()
			var value request
			ok := decode(w, r, &value)
			if ok != test.wantOK {
				t.Fatalf("decode returned %v", ok)
			}
			if !ok && w.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, test.wantStatus)
			}
		})
	}
}
