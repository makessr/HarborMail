package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestAdminMFAEnrollmentCLI(t *testing.T) {
	const owner = "admin+mail@harbor.test"
	const uriHeading = "Authenticator URI (secret):\n"
	const configHeading = "HARBOR_ADMIN_MFA configuration (secret):\n"
	const qrHeading = "Authenticator QR code (secret):\n"

	for _, showQR := range []bool{false, true} {
		args := []string{owner}
		if showQR {
			args = append(args, "--qr")
		}
		var output bytes.Buffer
		if err := runAdminMFAEnrollment(&output, args); err != nil {
			t.Fatal(err)
		}
		prefix, enrollment, found := strings.Cut(output.String(), uriHeading)
		if !found || (showQR && !strings.HasPrefix(prefix, qrHeading+"\x1b[30;107m")) || (!showQR && prefix != "") {
			t.Fatalf("unexpected enrollment output format for QR=%t", showQR)
		}
		uriText, rest, found := strings.Cut(enrollment, "\n\n"+configHeading)
		configText, recovery, hasRecovery := strings.Cut(rest, "\n\nSingle-use recovery codes:\n")
		uri, err := url.Parse(uriText)
		if !found || !hasRecovery || err != nil || uri.Scheme != "otpauth" || uri.Host != "totp" || uri.Path != "/Harbor Mail:"+owner {
			t.Fatal("invalid enrollment URI or missing configuration or recovery codes")
		}
		config, err := parseAdminMFA(configText, map[string]bool{owner: true})
		if err != nil || uri.Query().Get("secret") != totpBase32.EncodeToString(config[owner].secret) {
			t.Fatal("authenticator URI and administrator configuration must share the same seed")
		}
		codes := strings.Fields(recovery)
		if len(codes) != 8 {
			t.Fatal("expected eight recovery codes")
		}
		for _, code := range codes {
			if !config[owner].recovery[adminRecoveryHash(code)] {
				t.Fatal("recovery code is missing from administrator configuration")
			}
		}
	}

	var rejected bytes.Buffer
	if err := runAdminMFAEnrollment(&rejected, []string{owner, "--invalid"}); err == nil || rejected.Len() != 0 {
		t.Fatal("invalid arguments must be rejected before writing enrollment data")
	}
}

func TestAdminMFAQRText(t *testing.T) {
	modules := [][]bool{{false, true, false, true}, {false, false, true, true}, {false, true, false, false}}
	if got := adminMFAQRText(modules); got != "\x1b[30;107m ▀▄█\x1b[0m\n\x1b[30;107m ▀  \x1b[0m\n" {
		t.Fatal("terminal output must preserve both rows of QR modules and pad the final row")
	}
}

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
