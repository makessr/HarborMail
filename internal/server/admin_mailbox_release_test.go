package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersion/go-smtp"
	"golang.org/x/crypto/bcrypt"
)

func TestMailboxAddressRelease(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testMailboxAddressRelease(t, "sqlite", filepath.Join(t.TempDir(), "release.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testMailboxAddressRelease(t, "mysql", dsn) })
	}
}

func testMailboxAddressRelease(t *testing.T, driver, source string) {
	const password = "mailbox-release-test-password"
	domain := "release-" + strings.ToLower(strings.ReplaceAll(token()[:8], "_", "-")) + "x.test"
	admin, owner := "admin@"+domain, "demo@"+domain
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, dbDriver: driver, sharedMailboxEvents: driver == "mysql",
		accounts: map[string][]byte{admin: hash}, admins: map[string]bool{admin: true}}
	if err := a.registerConfiguredAccounts(); err != nil {
		t.Fatal(err)
	}
	handler := a.routes()
	var cookies []*http.Cookie
	call := func(method, path string, body map[string]string, want int) string {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: got %d, want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		if path == "/api/login" {
			cookies = w.Result().Cookies()
		}
		return w.Body.String()
	}
	call("POST", "/api/login", map[string]string{"email": admin, "password": password}, http.StatusOK)
	create := func(want int) string {
		t.Helper()
		return call("POST", "/api/admin/accounts", map[string]string{
			"email": owner, "initialPassword": password, "password": password,
		}, want)
	}
	lifecycle := func(action, confirm string, want int) {
		t.Helper()
		call("POST", "/api/admin/accounts/"+owner+"/lifecycle", map[string]string{
			"action": action, "confirm": confirm, "password": password,
		}, want)
	}
	create(http.StatusCreated)
	stale := &delivery{app: a, sender: "friend@external.test"}
	if err := stale.resolveRecipient(owner); err != nil {
		t.Fatal(err)
	}
	lifecycle("mailbox_retire", "", http.StatusNoContent)
	lifecycle("mailbox_purge", owner, http.StatusNoContent)
	if result := create(http.StatusConflict); !strings.Contains(result, "已回收") {
		t.Fatal(result)
	}
	if result := call("GET", "/api/admin/accounts", nil, http.StatusOK); !strings.Contains(result, `"purged":true,"canDelete":true`) {
		t.Fatal(result)
	}
	lifecycle("mailbox_delete", "incorrect", http.StatusBadRequest)
	lifecycle("mailbox_delete", owner, http.StatusNoContent)
	create(http.StatusCreated)
	const message = "From: friend@external.test\r\nSubject: test\r\n\r\nBody\r\n"
	var smtpError *smtp.SMTPError
	if err := stale.Data(strings.NewReader(message)); !errors.As(err, &smtpError) || smtpError.Code != 451 {
		t.Fatalf("old SMTP transaction reached the replacement mailbox: %v", err)
	}
	current := &delivery{app: a, sender: stale.sender}
	if err := current.resolveRecipient(owner); err != nil {
		t.Fatal(err)
	}
	if err := current.Data(strings.NewReader(message)); err != nil {
		t.Fatal(err)
	}
	result := call("GET", "/api/admin/storage", nil, http.StatusOK)
	var storage struct {
		Items []accountStorage
	}
	if err := json.Unmarshal([]byte(result), &storage); err != nil {
		t.Fatal(err)
	}
	for _, item := range storage.Items {
		if item.Email == owner && item.MessageCount == 1 && item.MessageBytes == int64(len(message)) {
			return
		}
	}
	t.Fatal(result)
}
