package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestInstallationAdminHandoffAndDomainRemoval(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testInstallationAdminHandoff(t, "sqlite", filepath.Join(t.TempDir(), "handoff.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testInstallationAdminHandoff(t, "mysql", dsn) })
	}
}

func testInstallationAdminHandoff(t *testing.T, driver, source string) {
	const password = "test-mailbox-password"
	suffix := strings.ToLower(token()[:10])
	oldDomain := "old-" + strings.ReplaceAll(suffix, "_", "-") + ".test"
	outgoing, incoming, alias := "setup@"+oldDomain, "owner@new-"+strings.ReplaceAll(suffix, "_", "-")+".test", "contact@"+oldDomain
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, dbDriver: driver, accounts: map[string][]byte{outgoing: hash},
		admins: map[string]bool{outgoing: true}, aliases: map[string]string{alias: outgoing}}
	if err := a.registerConfiguredAccounts(); err != nil {
		t.Fatal(err)
	}
	handler := a.routes()
	call := func(method, path string, body map[string]any, credentials []*http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		data, _ := json.Marshal(body)
		request := httptest.NewRequest(method, path, bytes.NewReader(data))
		request.Header.Set("Content-Type", "application/json")
		for _, cookie := range credentials {
			request.AddCookie(cookie)
		}
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		if result.Code != want {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, result.Code, want, result.Body.String())
		}
		return result
	}
	login := func(owner string, want int) []*http.Cookie {
		t.Helper()
		return call(http.MethodPost, "/api/login", map[string]any{"email": owner, "password": password}, nil, want).Result().Cookies()
	}
	lifecycle := "/api/admin/accounts/" + outgoing + "/lifecycle"
	change := func(action, confirm string, credentials []*http.Cookie, want int) {
		t.Helper()
		call(http.MethodPost, lifecycle, map[string]any{"action": action, "confirm": confirm, "password": password}, credentials, want)
	}
	domain := func(action string, credentials []*http.Cookie, want int) {
		t.Helper()
		call(http.MethodPost, "/api/admin/domains", map[string]any{"domain": oldDomain, "action": action, "password": password}, credentials, want)
	}
	original := login(outgoing, http.StatusOK)
	domain("domain_add", original, http.StatusCreated)
	domain("domain_remove", original, http.StatusConflict)
	change("mailbox_retire", "", original, http.StatusConflict)

	call(http.MethodPost, "/api/admin/accounts", map[string]any{
		"email": incoming, "initialPassword": password, "password": password,
	}, original, http.StatusCreated)
	call(http.MethodPost, "/api/admin/accounts/"+incoming+"/role", map[string]any{
		"admin": true, "password": password,
	}, original, http.StatusOK)
	change("mailbox_retire", "", original, http.StatusConflict)
	successor := login(incoming, http.StatusOK)
	change("mailbox_retire", "", successor, http.StatusForbidden)
	change("mailbox_retire", "", original, http.StatusNoContent)

	call(http.MethodGet, "/api/admin/overview", nil, original, http.StatusUnauthorized)
	call(http.MethodGet, "/api/admin/overview", nil, successor, http.StatusOK)
	domain("domain_remove", successor, http.StatusConflict)
	for _, address := range []string{outgoing, alias} {
		if _, err := a.destination(context.Background(), db, address); !errors.Is(err, errMailboxRetired) {
			t.Fatalf("retired recipient %s remained deliverable: %v", address, err)
		}
	}
	change("mailbox_purge", "incorrect", successor, http.StatusBadRequest)
	change("mailbox_purge", outgoing, successor, http.StatusNoContent)
	change("mailbox_restore", "", successor, http.StatusNotFound)
	domain("domain_remove", successor, http.StatusNoContent)

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	a.db, db = reopened, reopened
	if err := a.registerConfiguredAccounts(); err != nil {
		t.Fatal(err)
	}
	handler = a.routes()
	login(outgoing, http.StatusUnauthorized)
	successor = login(incoming, http.StatusOK)
	snapshot := call(http.MethodGet, "/api/admin/overview", nil, successor, http.StatusOK)
	if strings.Contains(snapshot.Body.String(), outgoing) || strings.Contains(snapshot.Body.String(), oldDomain) {
		t.Fatalf("deleted configuration returned after restart: %s", snapshot.Body.String())
	}
}
