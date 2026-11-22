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
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{
			"tag_name":"v0.2.14",
			"assets":[
				{"name":"harbormail-windows-amd64.exe","state":"uploaded"},
				{"name":"harbormail-windows-arm64.exe","state":"uploaded"},
				{"name":"harbormail-linux-amd64","state":"open"}
			]}`))}, nil
	})}}
	first, err := check.latest()
	second, cachedErr := check.latest()
	if first.Tag != "v0.2.14" || second.Tag != first.Tag || err != nil || cachedErr != nil || calls != 1 {
		t.Fatalf("unexpected cached release: %q, %q, %v, %v, calls=%d", first.Tag, second.Tag, err, cachedErr, calls)
	}
	for _, test := range []struct{ os, arch, kind, label, asset, download string }{
		{"windows", "amd64", "binary", "Windows AMD64 · 二进制", "harbormail-windows-amd64.exe", "harbormail-windows-amd64.exe"},
		{"windows", "arm64", "binary", "Windows ARM64 · 二进制", "harbormail-windows-arm64.exe", "harbormail-windows-arm64.exe"},
		{"linux", "arm64", "docker", "Docker · Linux ARM64", "", ""},
		{"linux", "amd64", "binary", "Linux AMD64 · 二进制", "harbormail-linux-amd64", ""},
	} {
		label, asset := deploymentDetails(test.os, test.arch, test.kind)
		if label != test.label || asset != test.asset || first.download(asset) != test.download {
			t.Errorf("%s/%s %s = %q, %q (download %q)", test.os, test.arch, test.kind, label, asset, first.download(asset))
		}
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
