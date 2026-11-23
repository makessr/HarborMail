package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDeploymentBackupStatus(t *testing.T) {
	now := time.Now()
	marker := filepath.Join(t.TempDir(), "harbor-backup.prom")
	for _, tc := range []struct {
		name, path, status string
		stamp              time.Time
	}{
		{"missing integration", "", "unverified", now},
		{"fresh", marker, "ok", now.Add(-time.Hour)},
		{"stale", marker, "warning", now.Add(-27 * time.Hour)},
		{"future", marker, "warning", now.Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := fmt.Sprintf("harbor_backup_last_success_timestamp_seconds %d\n", tc.stamp.Unix())
			if err := os.WriteFile(marker, []byte(record), 0600); err != nil {
				t.Fatal(err)
			}
			if check := deploymentBackup(tc.path, now); check.Status != tc.status {
				t.Fatalf("status=%s want %s", check.Status, tc.status)
			}
		})
	}
}

func TestDeploymentSecurityProbe(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/checkv2" || r.Header.Get("From") != "probe@harbor.test" {
			t.Error("unexpected probe request")
		}
		message, err := mail.ReadMessage(r.Body)
		if err != nil || message.Header.Get("Message-ID") == "" || message.Header.Get("Date") == "" || message.Header.Get("Content-Type") == "" {
			t.Error("probe requires the standard message headers")
		}
		fmt.Fprint(w, `{"action":"no action"}`)
	}))
	defer worker.Close()
	scanner, err := newRspamdScanner(worker.URL + "/checkv2")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{spamScanner: scanner, secure: true, scanPolicy: inboundScanPolicy{deferUnavailable: true}}
	endpoint := "/api/admin/deployment-security"
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, httptest.NewRequest("GET", endpoint, nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated result = %d", response.Code)
	}
	response = httptest.NewRecorder()
	a.adminDeploymentSecurity(response, httptest.NewRequest("GET", "https://harbor.test"+endpoint, nil), "admin@harbor.test")
	json := response.Body.String()
	for _, item := range []string{`"name":"HTTPS","status":"ok"`, `"name":"会话 Cookie","status":"ok"`, `"name":"来信扫描","status":"ok"`} {
		if !strings.Contains(json, item) || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("missing %s in %s", item, json)
		}
	}
	if got := a.inbound.snapshot(); got != (inboundUsage{}) {
		t.Fatalf("probe leaked capacity: %+v", got)
	}
}
