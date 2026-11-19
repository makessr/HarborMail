package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRecoveryModeWebRoutes(t *testing.T) {
	handler := (&app{recoveryMode: true}).routes()
	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodGet, "/api/letters", http.StatusUnauthorized},
		{http.MethodPost, "/api/login", http.StatusUnsupportedMediaType},
		{http.MethodPost, "/api/logout", http.StatusUnauthorized},
		{http.MethodPost, "/api/letters", http.StatusServiceUnavailable},
		{http.MethodPatch, "/api/letters/1", http.StatusServiceUnavailable},
		{http.MethodPost, "/api/drafts/1/send", http.StatusServiceUnavailable},
		{http.MethodPost, "/api/admin/queue/1/retry", http.StatusServiceUnavailable},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			result := httptest.NewRecorder()
			handler.ServeHTTP(result, httptest.NewRequest(tc.method, tc.path, nil))
			if result.Code != tc.want {
				t.Fatalf("status = %d, want %d", result.Code, tc.want)
			}
		})
	}
}
