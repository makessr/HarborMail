package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrowserSessionsKeepLegacyLoginsAndScopeRevocation(t *testing.T) {
	const owner = "owner@harbor.test"
	const foreign = "other@harbor.test"
	const currentCookie = "test-current-cookie"
	const legacyCookie = "test-legacy-cookie"
	const foreignCookie = "test-foreign-cookie"
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, accounts: map[string][]byte{owner: []byte("account hash"), foreign: []byte("different hash")}}
	now := time.Now()
	digests := make(map[string][32]byte)
	for cookie, account := range map[string]string{currentCookie: owner, legacyCookie: owner, foreignCookie: foreign} {
		digest := sha256.Sum256([]byte(cookie))
		digests[cookie] = digest
		if cookie == legacyCookie {
			credential := sha256.Sum256(a.accounts[account])
			if _, err := db.Exec(`INSERT INTO sessions (digest,owner,expires,credential) VALUES (?,?,?,?)`,
				digest[:], account, now.Add(time.Hour).Unix(), credential[:]); err != nil {
				t.Fatal(err)
			}
		} else {
			r := httptest.NewRequest(http.MethodPost, "/api/login", nil)
			r.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0) Chrome/100.0 Safari/537.36 Edg/100.0")
			if err := a.createBrowserSession(r, account, digest[:], a.accounts[account], ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	router := a.routes()
	call := func(method, path, cookie string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	for _, cookie := range []string{currentCookie, legacyCookie, foreignCookie} {
		if w := call(http.MethodGet, "/api/session", cookie); w.Code != http.StatusOK {
			t.Fatalf("stored session could not log in: HTTP %d", w.Code)
		}
	}
	digest := digests[currentCookie]
	if _, err := db.Exec(`UPDATE web_session_details SET last_active=? WHERE digest=?`, now.Add(-time.Hour).Unix(), digest[:]); err != nil {
		t.Fatal(err)
	}
	w := call(http.MethodGet, "/api/web-sessions", currentCookie)
	var sessions []browserSessionInfo
	if err := json.Unmarshal(w.Body.Bytes(), &sessions); err != nil || w.Code != http.StatusOK || len(sessions) != 2 {
		t.Fatalf("listing session data: %d: %v", w.Code, err)
	}
	if !sessions[0].Current || sessions[0].Device != "Edge · Windows" || sessions[0].LastActive < now.Unix() ||
		sessions[1].Current || sessions[1].Device != "" || strings.Contains(w.Body.String(), hex.EncodeToString(digest[:])) {
		t.Fatal("current device or legacy metadata was incorrect")
	}
	if w := call(http.MethodDelete, "/api/web-sessions/"+sessions[1].ID, foreignCookie); w.Code != http.StatusNoContent {
		t.Fatalf("foreign revocation status: %d", w.Code)
	}
	if w := call(http.MethodDelete, "/api/web-sessions/"+sessions[0].ID, currentCookie); w.Code != http.StatusNoContent {
		t.Fatalf("self revocation status: %d", w.Code)
	}
	if w := call(http.MethodGet, "/api/session", legacyCookie); w.Code != http.StatusOK {
		t.Fatal("foreign account affected legacy session")
	}
	if w := call(http.MethodDelete, "/api/web-sessions", currentCookie); w.Code != http.StatusNoContent {
		t.Fatalf("revoking other sessions: %d", w.Code)
	}
	if w := call(http.MethodGet, "/api/session", legacyCookie); w.Code != http.StatusUnauthorized {
		t.Fatal("legacy session was not revoked")
	}
	stale := httptest.NewRequest(http.MethodDelete, "/api/web-sessions", nil)
	stale.AddCookie(&http.Cookie{Name: cookieName, Value: legacyCookie})
	if err := a.deleteBrowserSessions(stale, owner, nil); !errors.Is(err, errBrowserSessionEnded) {
		t.Fatalf("revoked session could mutate the active account: %v", err)
	}
	if w := call(http.MethodGet, "/api/session", currentCookie); w.Code != http.StatusOK {
		t.Fatal("bulk revocation removed the calling session")
	}
	if w := call(http.MethodPost, "/api/logout", currentCookie); w.Code != http.StatusNoContent {
		t.Fatalf("ordinary logout failed: %d", w.Code)
	}
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM web_session_details WHERE owner=?`, owner).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("logout left active metadata: %d, %v", remaining, err)
	}
	if w := call(http.MethodGet, "/api/session", foreignCookie); w.Code != http.StatusOK {
		t.Fatal("bulk revocation affected another account")
	}
}
