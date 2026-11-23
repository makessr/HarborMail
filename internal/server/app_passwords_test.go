package server

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestAppPasswordsEnforceAccountScopeExpiryAndRotation(t *testing.T) {
	const owner = "owner@harbor.test"
	const other = "other@harbor.test"
	const primary = "primary-test-password"
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hashPassword := func() []byte {
		t.Helper()
		hash, err := bcrypt.GenerateFromPassword([]byte(primary), bcrypt.MinCost)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	a := &app{db: db, accounts: map[string][]byte{owner: hashPassword(), other: hashPassword()}}
	value := token()
	digest := sha256.Sum256([]byte(value))
	if err := a.createBrowserSession(httptest.NewRequest(http.MethodPost, "/api/login", nil), owner, digest[:], a.accounts[owner], ""); err != nil {
		t.Fatal(err)
	}
	create := func(password string) *httptest.ResponseRecorder {
		t.Helper()
		body := `{"label":"Laptop","password":"` + password + `","days":30,"protocols":["imap"]}`
		req := httptest.NewRequest(http.MethodPost, "/api/app-passwords", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: cookieName, Value: value})
		resp := httptest.NewRecorder()
		a.createAppPassword(resp, req, owner)
		return resp
	}
	if response := create("wrong"); response.Code != http.StatusForbidden {
		t.Fatalf("issuing without reauthentication: HTTP %d", response.Code)
	}
	newPassword := func() (id int64, secret string) {
		t.Helper()
		response := create(primary)
		var payload struct {
			ID     int64
			Secret string
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil ||
			response.Code != http.StatusCreated || !isAppPassword(payload.Secret) {
			t.Fatalf("issuing app password: HTTP %d, %v", response.Code, err)
		}
		return payload.ID, payload.Secret
	}
	id, secret := newPassword()
	mustFail := func(account, password string, protocol mailProtocol) {
		t.Helper()
		if _, _, err := a.authenticateClient(account, password, protocol); !errors.Is(err, errLoginInvalid) {
			t.Fatalf("unexpected auth result: %v", err)
		}
	}
	mustFail(owner, secret, clientSMTP)
	mustFail(other, secret, clientIMAP)
	if _, err := a.authenticate(owner, secret); !errors.Is(err, errLoginInvalid) {
		t.Fatal("app passwords should not authenticate on the Web")
	}
	revoke := func(account string, id int64) {
		t.Helper()
		req := httptest.NewRequest(http.MethodDelete, "/api/app-passwords", nil)
		req.SetPathValue("id", strconv.FormatInt(id, 10))
		resp := httptest.NewRecorder()
		a.revokeAppPassword(resp, req, account)
		if resp.Code != http.StatusNoContent {
			t.Fatalf("revoking: HTTP %d", resp.Code)
		}
	}
	revoke(other, id)
	if _, _, err := a.authenticateClient(owner, secret, clientIMAP); err != nil {
		t.Fatalf("valid credential failed after another account attempted revocation: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/app-passwords", nil)
	resp := httptest.NewRecorder()
	a.listAppPasswords(resp, req, owner)
	var listed []appPassword
	if err := json.Unmarshal(resp.Body.Bytes(), &listed); resp.Code != http.StatusOK || err != nil ||
		len(listed) != 1 || listed[0].LastUsed == 0 || strings.Contains(resp.Body.String(), secret) {
		t.Fatalf("unexpected list metadata: HTTP %d, %v", resp.Code, err)
	}
	if _, err := db.Exec(`UPDATE app_passwords SET expires=1 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	mustFail(owner, secret, clientIMAP)
	id, secret = newPassword()
	revoke(owner, id)
	mustFail(owner, secret, clientIMAP)
	_, secret = newPassword()
	a.accounts[owner] = hashPassword()
	mustFail(owner, secret, clientIMAP)
	if _, _, err := a.authenticateClient(owner, primary, clientSMTP); err != nil {
		t.Fatalf("primary password stopped working: %v", err)
	}
}
