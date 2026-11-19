package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestAdminMFAUsesMySQLTime(t *testing.T) {
	dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set HARBOR_TEST_MYSQL_DSN to a disposable MySQL database")
	}
	db, err := openDB("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const owner = "mfa-clock@harbor.test"
	seed := []byte("12345678901234567890")
	a := &app{db: db, dbDriver: "mysql", adminMFA: map[string]adminMFA{
		owner: {secret: seed, enrollment: sha256.Sum256(seed)},
	}}
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var unix int64
	if err := tx.QueryRowContext(ctx, "SELECT UNIX_TIMESTAMP()").Scan(&unix); err != nil {
		t.Fatal(err)
	}
	code := adminTOTP(seed, unix/30)
	if err := a.consumeAdminMFA(ctx, tx, owner, code, time.Unix(0, 0)); err != nil {
		t.Fatalf("database-time TOTP rejected despite valid proof: %v", err)
	}
}

func TestAdminMFARequiresFreshProofAndInvalidatesOldSession(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const owner, password = "admin@harbor.test", "administrator-test-password"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, accounts: map[string][]byte{owner: hash}, admins: map[string]bool{owner: true}}
	signIn := func(code string, status int) []*http.Cookie {
		t.Helper()
		payload, _ := json.Marshal(map[string]string{"email": owner, "password": password, "code": code})
		request := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		a.login(response, request)
		if response.Code != status {
			t.Fatalf("login returned %d, want %d: %s", response.Code, status, response.Body.String())
		}
		return response.Result().Cookies()
	}
	old := signIn("", http.StatusOK)
	const recovery = "30303030303030303030303030303030"
	secret := []byte("12345678901234567890")
	if got := adminTOTP(secret, 59/30); got != "287082" {
		t.Fatalf("RFC 6238 SHA-1 vector: %s", got)
	}
	recoveryHash := adminRecoveryHash(recovery)
	raw, _ := json.Marshal(map[string]adminMFAConfig{owner: {
		Secret: totpBase32.EncodeToString(secret), RecoveryHashes: []string{hex.EncodeToString(recoveryHash[:])},
	}})
	a.adminMFA, err = parseAdminMFA(string(raw), a.admins)
	if err != nil {
		t.Fatal(err)
	}
	checkSession := func(cookies []*http.Cookie, expected string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/session", nil)
		for _, cookie := range cookies {
			request.AddCookie(cookie)
		}
		session, err := a.browserSession(request)
		if err != nil || session.owner != expected {
			t.Fatalf("session owner = %q, want %q, err=%v", session.owner, expected, err)
		}
	}
	checkSession(old, "")
	signIn("", http.StatusPreconditionRequired)
	code := adminTOTP(secret, time.Now().Unix()/30)
	checkSession(signIn(code, http.StatusOK), owner)
	signIn(code, http.StatusPreconditionRequired)
	checkSession(signIn(recovery, http.StatusOK), owner)
	signIn(recovery, http.StatusPreconditionRequired)
}
