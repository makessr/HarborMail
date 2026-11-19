package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileSetting(t *testing.T) {
	const key = "HARBOR_TEST_SETTING"
	path := filepath.Join(t.TempDir(), "setting")
	if err := os.WriteFile(path, []byte("example with spaces \r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(key, "")
	t.Setenv(key+"_FILE", path)
	value, err := fileSetting(key)
	if err != nil || value != "example with spaces " {
		t.Fatalf("file setting was not read as expected: %v", err)
	}
	t.Setenv(key, "environment example")
	if _, err := fileSetting(key); err == nil || strings.Contains(err.Error(), "environment example") {
		t.Fatalf("ambiguous setting must be rejected without echoing the value: %v", err)
	}
}

func TestHealthReportsDatabaseAvailability(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	router := (&app{db: db}).routes()
	status := func() int {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return w.Code
	}
	if got := status(); got != http.StatusOK {
		t.Fatalf("health with database available = %d", got)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != http.StatusServiceUnavailable {
		t.Fatalf("health with database unavailable = %d", got)
	}
}
