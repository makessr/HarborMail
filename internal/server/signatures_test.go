package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignatureSettings(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { testSignatureSettings(t, "sqlite", filepath.Join(t.TempDir(), "mail.db")) })
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testSignatureSettings(t, "mysql", dsn) })
	}
}

func testSignatureSettings(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db}
	owner := token()[:16] + "@signature.harbor.test"
	call := func(method, actor, body string, want int) signatureSettings {
		t.Helper()
		r := httptest.NewRequest(method, "/api/signatures", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		if method == http.MethodGet {
			a.getSignatures(w, r, actor)
		} else {
			a.putSignatures(w, r, actor)
		}
		if w.Code != want {
			t.Fatalf("%s signatures: got %d, want %d: %s", method, w.Code, want, w.Body)
		}
		var settings signatureSettings
		if want == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &settings); err != nil {
				t.Fatal(err)
			}
		}
		return settings
	}
	if empty := call("GET", owner, "", 200); empty.Revision != 0 || empty.Signatures == nil || len(empty.Signatures) != 0 {
		t.Fatalf("initial settings: %#v", empty)
	}
	const id = "0123456789abcdef0123456789abcdef"
	const otherID = "abcdef0123456789abcdef0123456789"
	const payload = `{"revision":0,"signatures":[{"id":"` + id + `","name":" Work ","body":"测试用户\r\nHarbor 团队\n"}],"defaults":{"compose":"` + id + `","reply":"","forward":"` + id + `"}}`
	settings := call("PUT", owner, payload, 200)
	if settings.Revision != 1 || len(settings.Signatures) != 1 || settings.Signatures[0].Name != "Work" || settings.Signatures[0].Body != "测试用户\nHarbor 团队" || settings.Defaults.Forward != id {
		t.Fatalf("stored settings: %#v", settings)
	}
	call("PUT", owner, payload, 409)
	call("PUT", owner, `{"revision":1,"signatures":[{"id":"`+id+`","name":"Work","body":"one"},{"id":"`+otherID+`","name":"work","body":"two"}]}`, 400)
	call("PUT", owner, `{"revision":1,"signatures":[],"defaults":{"reply":"`+id+`"}}`, 400)
	other := "other-" + owner
	if foreign := call("GET", other, "", 200); foreign.Revision != 0 || len(foreign.Signatures) != 0 {
		t.Fatalf("signature settings leaked across accounts: %#v", foreign)
	}
	call("PUT", other, `{"revision":1}`, 409)
	if latest := call("GET", owner, "", 200); latest.Revision != 1 || latest.Defaults.Compose != id || len(latest.Signatures) != 1 {
		t.Fatalf("failed writes changed settings: %#v", latest)
	}
	call("PUT", owner, `{"revision":1,"signatures":[]}`, 200)
	if empty := call("GET", owner, "", 200); empty.Revision != 2 || len(empty.Signatures) != 0 || empty.Defaults != (signatureDefaults{}) {
		t.Fatalf("deleted signatures remain configured: %#v", empty)
	}
}
