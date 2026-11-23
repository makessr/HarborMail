package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestWebMailboxPasswordRecovery(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testWebMailboxPasswordRecovery(t, "sqlite", filepath.Join(t.TempDir(), "recovery.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testWebMailboxPasswordRecovery(t, "mysql", dsn) })
	}
}

func testWebMailboxPasswordRecovery(t *testing.T, driver, source string) {
	const initial = "fixture-initial-password"
	const changed = "fixture-changed-password"
	const recovered = "fixture-recovered-password"
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := "recovery-" + token()[:8] + "@harbor.test"
	base, err := bcrypt.GenerateFromPassword([]byte(initial), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, dbDriver: driver, accounts: map[string][]byte{owner: base}}
	handler := a.routes()
	peer := "192.0.2.10:8192"
	request := func(path string, payload map[string]string, cookies []*http.Cookie, want int) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(payload)
		r := httptest.NewRequest("POST", "/api/"+path, bytes.NewReader(body))
		r.RemoteAddr = peer
		r.Header.Set("Content-Type", "application/json")
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("POST /api/%s returned %d, want %d", path, w.Code, want)
		}
		return w
	}
	login := func(password string, want int) []*http.Cookie {
		t.Helper()
		return request("login", map[string]string{"email": owner, "password": password}, nil, want).Result().Cookies()
	}
	issue := func(password string, session []*http.Cookie) string {
		t.Helper()
		w := request("password/recovery-code", map[string]string{"password": password}, session, http.StatusCreated)
		var result struct{ Code string }
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Code) != 64 {
			t.Fatal("could not issue a recovery code")
		}
		return result.Code
	}
	recover := func(email, code string, want int) *httptest.ResponseRecorder {
		t.Helper()
		return request("password/recover", map[string]string{"email": email, "code": code, "newPassword": recovered}, nil, want)
	}
	session := login(initial, http.StatusOK)
	request("password/recovery-code", map[string]string{"password": initial}, nil, http.StatusUnauthorized)
	request("password/recovery-code", map[string]string{"password": "wrong"}, session, http.StatusForbidden)
	obsolete := issue(initial, session)
	request("password", map[string]string{"password": initial, "newPassword": changed}, session, http.StatusOK)
	request("password/recovery-code", map[string]string{"password": changed}, session, http.StatusUnauthorized)
	login(initial, http.StatusUnauthorized)
	session = login(changed, http.StatusOK)
	recover(owner, obsolete, http.StatusForbidden)
	peer = "192.0.2.11:8192"
	superseded := issue(changed, session)
	active := issue(changed, session)
	recover(owner, superseded, http.StatusForbidden)
	unknown := recover("nonexistent-"+owner, active, http.StatusForbidden)
	invalid := recover(owner, strings.Repeat("0", 64), http.StatusForbidden)
	if unknown.Body.String() != invalid.Body.String() {
		t.Fatal("recovery response revealed whether the mailbox exists")
	}
	recover(owner, active, http.StatusNoContent)
	recover(owner, active, http.StatusForbidden)
	request("password/recovery-code", map[string]string{"password": recovered}, session, http.StatusUnauthorized)
	login(changed, http.StatusUnauthorized)
	login(recovered, http.StatusOK)
}
