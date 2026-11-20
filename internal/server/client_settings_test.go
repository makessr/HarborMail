package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientSettingsUsesPublicPortsForEnabledListeners(t *testing.T) {
	t.Setenv("HARBOR_CLIENT_HOST", "MAIL.HARBOR.TEST")
	t.Setenv("HARBOR_IMAPS", "127.0.0.1:2993")
	t.Setenv("HARBOR_IMAPS_PORT", "993")
	t.Setenv("HARBOR_POP3S", "")
	t.Setenv("HARBOR_SUBMISSION", "127.0.0.1:2587")
	t.Setenv("HARBOR_SUBMISSION_PORT", "")
	t.Setenv("HARBOR_SUBMISSIONS", "")
	w := httptest.NewRecorder()
	new(app).clientSettings(w, httptest.NewRequest("GET", "/api/client-settings", nil), "person@harbor.test")
	var config struct {
		Host, Username string
		Protocols      []clientProtocol
	}
	if err := json.Unmarshal(w.Body.Bytes(), &config); err != nil || !strings.EqualFold(config.Host, "mail.harbor.test") ||
		config.Username != "person@harbor.test" || len(config.Protocols) != 2 ||
		config.Protocols[0].Port != 993 || config.Protocols[1].Port != 2587 || config.Protocols[1].TLS != "STARTTLS" {
		t.Fatalf("unexpected client settings: %+v, %v", config, err)
	}
}

func TestAdminDomainOriginalVerifiesOnlyKnownDomain(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "domains.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const actor = "admin@harbor.test"
	a := &app{db: db, admins: map[string]bool{actor: true}, accounts: map[string][]byte{actor: {}}, domain: "harbor.test"}
	for _, tc := range []struct {
		domain, actor, body string
		status              int
	}{
		{"harbor.test", actor, "From: admin@harbor.test\r\nSubject: unsigned\r\n\r\nBody\r\n", http.StatusOK},
		{"harbor.test", actor, "", http.StatusBadRequest},
		{"unknown.test", actor, "", http.StatusNotFound},
		{"harbor.test", "outsider@harbor.test", "", http.StatusForbidden},
	} {
		r := httptest.NewRequest("POST", "/api/admin/domains/verify", strings.NewReader(tc.body))
		r.SetPathValue("domain", tc.domain)
		w := httptest.NewRecorder()
		a.adminVerifyDomainMail(w, r, tc.actor)
		if w.Code != tc.status || (w.Code == http.StatusOK && !strings.Contains(w.Body.String(), `"status":"no_signature"`)) {
			t.Fatalf("%s (%s): %d, %s", tc.domain, tc.actor, w.Code, w.Body)
		}
	}
}
