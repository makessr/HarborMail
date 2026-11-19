package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMailTemplates(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { testMailTemplates(t, "sqlite", filepath.Join(t.TempDir(), "mail.db")) })
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testMailTemplates(t, "mysql", dsn) })
	}
}

func testMailTemplates(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db}
	owner := token()[:16] + "@templates.harbor.test"
	otherOwner := "other-" + owner
	call := func(method, actor, body string, status int) mailTemplates {
		t.Helper()
		r := httptest.NewRequest(method, "/api/templates", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		if method == http.MethodGet {
			a.getMailTemplates(w, r, actor)
		} else {
			a.putMailTemplates(w, r, actor)
		}
		if w.Code != status {
			t.Fatalf("%s: got %d, want %d: %s", method, w.Code, status, w.Body)
		}
		var settings mailTemplates
		if status == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &settings); err != nil {
				t.Fatal(err)
			}
		}
		return settings
	}

	if initial := call("GET", owner, "", 200); initial.Revision != 0 || initial.Templates == nil || len(initial.Templates) != 0 {
		t.Fatalf("initial templates: %#v", initial)
	}
	const create = `{"revision":0,"templates":[{"name":"  Quote  ","subject":" 报价 📄 ","body":"您好，\r\n\t请查收报价。\n"}]}`
	saved := call("PUT", owner, create, 200)
	want := mailTemplate{Name: "Quote", Subject: "报价 📄", Body: "您好，\n\t请查收报价。\n"}
	if saved.Revision != 1 || len(saved.Templates) != 1 || saved.Templates[0] != want {
		t.Fatalf("saved templates: %#v", saved)
	}
	call("PUT", owner, create, 409)
	call("PUT", otherOwner, `{"revision":1,"templates":[]}`, 409)
	if other := call("GET", otherOwner, "", 200); other.Revision != 0 || len(other.Templates) != 0 {
		t.Fatalf("templates shared with another account: %#v", other)
	}
	for _, body := range []string{
		`{"revision":1}`,
		`{"revision":1,"templates":[{"name":"Quote","body":"one"},{"name":"quote","body":"two"}]}`,
		`{"revision":1,"templates":[{"name":"Quote","subject":"one\ntwo","body":"content"}]}`,
		`{"revision":1,"templates":[{"name":"Quote","body":"\n\t "}]}`,
		`{"revision":1,"templates":[{"name":"Quote","body":"` + strings.Repeat("字", maxTemplateBytes/3+1) + `"}]}`,
	} {
		call("PUT", owner, body, 400)
	}
	if latest := call("GET", owner, "", 200); latest.Revision != 1 || len(latest.Templates) != 1 || latest.Templates[0] != want {
		t.Fatalf("invalid requests modified templates: %#v", latest)
	}
	const edit = `{"revision":1,"templates":[{"name":"Updated","body":"New body"}]}`
	updated := call("PUT", owner, edit, 200)
	if updated.Revision != 2 || len(updated.Templates) != 1 || updated.Templates[0].Name != "Updated" {
		t.Fatalf("updated templates: %#v", updated)
	}
	call("PUT", owner, `{"revision":1,"templates":[]}`, 409)
	call("PUT", owner, `{"revision":2,"templates":[]}`, 200)
	if removed := call("GET", owner, "", 200); removed.Revision != 3 || removed.Templates == nil || len(removed.Templates) != 0 {
		t.Fatalf("deleted templates still present: %#v", removed)
	}
}
