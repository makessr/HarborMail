package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestMailboxMFA(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testMailboxMFA(t, "sqlite", filepath.Join(t.TempDir(), "mail.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testMailboxMFA(t, "mysql", dsn) })
	}
}

func testMailboxMFA(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := token()[:12] + "@harbor.test"
	const password = "mailbox-test-password"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, dbDriver: driver, accounts: map[string][]byte{owner: hash}, dkimCipher: testDKIMCipher(t)}
	routes := a.routes()
	requestNumber := 0
	call := func(method, path string, cookies []*http.Cookie, input any, want int) *httptest.ResponseRecorder {
		t.Helper()
		payload, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, "/api/"+path, bytes.NewReader(payload))
		requestNumber++
		req.RemoteAddr = fmt.Sprintf("192.0.2.%d:50000", requestNumber)
		req.Header.Set("Content-Type", "application/json")
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		routes.ServeHTTP(w, req)
		if w.Code != want {
			// Responses can contain credentials. Do not log their bodies.
			t.Fatalf("%s %s returned %d, want %d", method, path, w.Code, want)
		}
		return w
	}
	signIn := func(code string, status int) []*http.Cookie {
		t.Helper()
		return call("POST", "login", nil, map[string]string{
			"email": owner, "password": password, "code": code,
		}, status).Result().Cookies()
	}
	current := signIn("", http.StatusOK)
	other := signIn("", http.StatusOK)
	if _, _, err := a.authenticateClient(owner, password, clientIMAP); err != nil {
		t.Fatalf("password should work before enrollment: %v", err)
	}
	var appCredential struct{ Secret string }
	appPassword := call("POST", "app-passwords", current, map[string]any{
		"label": "Test client", "protocols": []string{"imap"}, "password": password,
	}, http.StatusCreated)
	if err := json.Unmarshal(appPassword.Body.Bytes(), &appCredential); err != nil || !isAppPassword(appCredential.Secret) {
		t.Fatal("app password was not issued")
	}
	var setup struct {
		Setup  string
		Secret string
		QR     [][]bool
	}
	prepared := call("POST", "two-factor/setup", current, nil, http.StatusOK)
	if err := json.Unmarshal(prepared.Body.Bytes(), &setup); err != nil || setup.Setup == "" || len(setup.QR) == 0 {
		t.Fatal("missing enrollment data")
	}
	seed, err := totpBase32.DecodeString(setup.Secret)
	if err != nil {
		t.Fatal("missing TOTP seed")
	}
	signIn("", http.StatusOK) // Preparing enrollment has no effect on account access.
	now := time.Now().Unix()
	if driver == "mysql" {
		if err := db.QueryRow("SELECT UNIX_TIMESTAMP()").Scan(&now); err != nil {
			t.Fatal(err)
		}
	}
	code := adminTOTP(seed, now/30)
	activation := map[string]string{"password": password, "setup": setup.Setup, "code": code}
	call("POST", "two-factor", other, activation, http.StatusConflict)
	call("POST", "two-factor", current,
		map[string]string{"password": "invalid", "setup": setup.Setup, "code": code}, http.StatusForbidden)
	result := call("POST", "two-factor", current, activation, http.StatusOK)
	var enabled struct {
		Enabled       bool
		RecoveryCodes []string
	}
	if err := json.Unmarshal(result.Body.Bytes(), &enabled); err != nil || !enabled.Enabled || len(enabled.RecoveryCodes) != 8 {
		t.Fatal("missing one-time recovery codes")
	}
	entry, err := readMailboxMFA(t.Context(), db, owner)
	if err != nil || !entry.enabled || bytes.Contains(entry.sealedSeed, seed) ||
		bytes.Contains([]byte(entry.recoveryHashes), []byte(enabled.RecoveryCodes[0])) {
		t.Fatal("two-factor enrollment was not stored in encrypted and hashed form")
	}
	call("GET", "session", current, nil, http.StatusOK)
	call("GET", "session", other, nil, http.StatusUnauthorized)
	signIn("", http.StatusPreconditionRequired)
	signIn(code, http.StatusPreconditionRequired)
	signIn(adminTOTP(seed, now/30+1), http.StatusOK)
	signIn(enabled.RecoveryCodes[0], http.StatusOK)
	signIn(enabled.RecoveryCodes[0], http.StatusPreconditionRequired)
	if _, _, err := a.authenticateClient(owner, password, clientIMAP); !errors.Is(err, errLoginInvalid) {
		t.Fatalf("primary password bypassed two-factor authentication: %v", err)
	}
	if _, _, err := a.authenticateClient(owner, appCredential.Secret, clientIMAP); err != nil {
		t.Fatalf("existing app password was rejected: %v", err)
	}
	call("DELETE", "two-factor", current, map[string]string{
		"password": password, "code": enabled.RecoveryCodes[0],
	}, http.StatusPreconditionRequired)
	call("DELETE", "two-factor", current, map[string]string{
		"password": password, "code": enabled.RecoveryCodes[1],
	}, http.StatusOK)
	signIn("", http.StatusOK)
	if _, _, err := a.authenticateClient(owner, password, clientIMAP); err != nil {
		t.Fatalf("password should work after disabling: %v", err)
	}
}
