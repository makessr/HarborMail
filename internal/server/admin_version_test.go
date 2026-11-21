package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminUpdateCheck(t *testing.T) {
	for _, tc := range []struct{ installed, latest, want string }{
		{"dev", "v0.2.14", "development"},
		{"v0.2.9", "v0.2.14", "available"},
		{"v0.2.14", "v0.2.14", "current"},
		{"v0.10.0", "v0.2.14", "ahead"},
	} {
		if status := updateStatus(tc.installed, tc.latest); status != tc.want {
			t.Errorf("version %s vs %s = %s, want %s", tc.installed, tc.latest, status, tc.want)
		}
	}

	calls := 0
	check := releaseCheck{client: &http.Client{Transport: translationTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet || req.URL.String() != latestReleaseAPI || req.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected GitHub request: %s %s", req.Method, req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"tag_name":"v0.2.14"}`))}, nil
	})}}
	first, err := check.latest()
	second, cachedErr := check.latest()
	if first != "v0.2.14" || second != first || err != nil || cachedErr != nil || calls != 1 {
		t.Fatalf("unexpected cached release: %q, %q, %v, %v, calls=%d", first, second, err, cachedErr, calls)
	}

	for _, item := range []string{`{"tag_name":"v0.2.14","prerelease":true}`, `{"tag_name":"v0.2.14","draft":true}`, `{"tag_name":"v0.2.14<script>"}`} {
		client := &http.Client{Transport: translationTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(item))}, nil
		})}
		if _, err := fetchStableRelease(client); err == nil {
			t.Errorf("accepted invalid release: %s", item)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/api/admin/about/update", nil)
	response := httptest.NewRecorder()
	(&app{}).routes().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated update check returned %d", response.Code)
	}
}
