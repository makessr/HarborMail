package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestAdminChangesOwnPassword(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testAdminChangesOwnPassword(t, "sqlite", filepath.Join(t.TempDir(), "admin-password.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testAdminChangesOwnPassword(t, "mysql", dsn) })
	}
}

func testAdminChangesOwnPassword(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const oldPassword = "fixture-original-password"
	const newPassword = "fixture-updated-password"
	admin, peer, user, scoped := "admin-"+token()[:8]+"@harbor.test", "peer-"+token()[:8]+"@harbor.test",
		"user-"+token()[:8]+"@harbor.test", "domain-"+token()[:8]+"@harbor.test"
	base, err := bcrypt.GenerateFromPassword([]byte(oldPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{
		db: db, dbDriver: driver,
		accounts:     map[string][]byte{admin: base, peer: base, user: base, scoped: base},
		admins:       map[string]bool{admin: true, peer: true},
		domainAdmins: map[string]map[string]bool{scoped: {"harbor.test": true}},
	}
	handler := a.routes()
	call := func(method, path string, payload map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	login := func(account string) []*http.Cookie {
		t.Helper()
		w := call(http.MethodPost, "/api/login", map[string]string{"email": account, "password": oldPassword})
		if w.Code != http.StatusOK {
			t.Fatalf("fixture login returned HTTP %d", w.Code)
		}
		return w.Result().Cookies()
	}
	payload := map[string]string{"password": oldPassword, "newPassword": newPassword, "email": peer}
	first, other := login(admin), login(admin)
	regular := login(user)
	check := func(w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != want {
			t.Fatalf("HTTP %d, want %d: %s", w.Code, want, w.Body)
		}
	}
	check(call(http.MethodPost, "/api/admin/password", payload, regular...), http.StatusForbidden)
	check(call(http.MethodPost, "/api/admin/accounts/"+peer+"/password", payload, first...), http.StatusForbidden)
	check(call(http.MethodPost, "/api/admin/password", map[string]string{"password": "incorrect", "newPassword": newPassword}, first...), http.StatusForbidden)

	issue, _ := json.Marshal(map[string]any{"label": "Fixture", "password": oldPassword, "days": 30, "protocols": []string{"imap"}})
	issueRequest := httptest.NewRequest(http.MethodPost, "/api/app-passwords", bytes.NewReader(issue))
	issueRequest.Header.Set("Content-Type", "application/json")
	for _, cookie := range first {
		issueRequest.AddCookie(cookie)
	}
	issued := httptest.NewRecorder()
	handler.ServeHTTP(issued, issueRequest)
	check(issued, http.StatusCreated)

	changed := call(http.MethodPost, "/api/admin/password", payload, first...)
	check(changed, http.StatusOK)
	var result struct {
		Affected  int
		SignedOut bool
	}
	if err := json.Unmarshal(changed.Body.Bytes(), &result); err != nil || !result.SignedOut || result.Affected != 3 {
		t.Fatalf("unexpected rotation result: %+v (%v)", result, err)
	}
	if cookies := changed.Result().Cookies(); len(cookies) != 1 || cookies[0].MaxAge != -1 {
		t.Fatal("password change did not expire the browser credential")
	}
	for _, session := range [][]*http.Cookie{first, other} {
		check(call(http.MethodGet, "/api/admin/overview", nil, session...), http.StatusUnauthorized)
	}
	if _, err := a.authenticate(admin, oldPassword); !errors.Is(err, errLoginInvalid) {
		t.Fatal("old password still works")
	}
	if _, err := a.authenticate(admin, newPassword); err != nil {
		t.Fatal("new password rejected")
	}
	if _, err := a.authenticate(peer, oldPassword); err != nil {
		t.Fatal("another administrator's password was modified")
	}
	var audit int
	if err := db.QueryRow("SELECT COUNT(*) FROM admin_account_actions WHERE actor=? AND target=? AND action='password_change'", admin, admin).Scan(&audit); err != nil || audit != 1 {
		t.Fatalf("password change audit count=%d (%v)", audit, err)
	}
	check(call(http.MethodPost, "/api/admin/password", payload, login(scoped)...), http.StatusOK)
	peerSession := login(peer)
	rolePath := "/api/admin/accounts/" + user + "/role"
	setRole := func(value bool, credentials []*http.Cookie, path string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"admin": value, "password": oldPassword})
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for _, cookie := range credentials {
			req.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	check(setRole(true, regular, rolePath), http.StatusForbidden)
	check(setRole(false, peerSession, "/api/admin/accounts/"+admin+"/role"), http.StatusForbidden)
	check(setRole(true, peerSession, rolePath), http.StatusOK)
	check(call(http.MethodGet, "/api/admin/overview", nil, regular...), http.StatusUnauthorized)
	delegated := login(user)
	check(call(http.MethodGet, "/api/admin/overview", nil, delegated...), http.StatusOK)
	check(setRole(true, delegated, "/api/admin/accounts/"+scoped+"/role"), http.StatusForbidden)
	check(setRole(false, peerSession, rolePath), http.StatusOK)
	check(call(http.MethodGet, "/api/admin/overview", nil, delegated...), http.StatusUnauthorized)
	check(call(http.MethodGet, "/api/admin/overview", nil, login(user)...), http.StatusForbidden)
}
