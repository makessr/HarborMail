package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestMailboxPreferences(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { testMailboxPreferences(t, "sqlite", filepath.Join(t.TempDir(), "mail.db")) })
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testMailboxPreferences(t, "mysql", dsn) })
	}
}

func testMailboxPreferences(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db}
	owner := token()[:16] + "@preferences.harbor.test"
	other := "other-" + owner
	call := func(method, actor string, input mailboxPreferences, status int) mailboxPreferences {
		t.Helper()
		payload, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(method, "/api/preferences", bytes.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		if method == http.MethodGet {
			a.getMailboxPreferences(w, r, actor)
		} else {
			a.putMailboxPreferences(w, r, actor)
		}
		if w.Code != status {
			t.Fatalf("%s preferences: got %d, want %d: %s", method, w.Code, status, w.Body)
		}
		var result mailboxPreferences
		if status == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	defaults := mailboxPreferences{Theme: "light", Density: "comfortable", ReadMode: "auto"}
	if result := call("GET", owner, mailboxPreferences{}, 200); result != defaults {
		t.Fatalf("default preferences: %+v", result)
	}
	for _, invalid := range []mailboxPreferences{
		{Theme: "", Density: "comfortable", ReadMode: "auto"},
		{Theme: "light", Density: "dense", ReadMode: "auto"},
		{Theme: "dark", Density: "compact", ReadMode: ""},
		{Theme: "dark", Density: "compact", ReadMode: "manual", UndoSeconds: 15},
	} {
		call("PUT", owner, invalid, 400)
	}
	subscription := a.events.watch(owner)
	selected := mailboxPreferences{Theme: "dark", Density: "compact", ReadMode: "manual", UndoSeconds: 10}
	saved := call("PUT", owner, selected, 200)
	selected.Revision = 1
	if saved != selected || call("GET", owner, mailboxPreferences{}, 200) != selected {
		t.Fatalf("saved preferences: %+v", saved)
	}
	select {
	case <-subscription:
	default:
		t.Fatal("preferences change did not notify subscribers")
	}
	call("PUT", owner, defaults, 409)
	call("PUT", other, selected, 409)
	if result := call("GET", other, mailboxPreferences{}, 200); result != defaults {
		t.Fatalf("other account preferences: %+v", result)
	}
	selected.Theme = "system"
	updated := call("PUT", owner, selected, 200)
	if updated.Revision != 2 || updated.Theme != "system" {
		t.Fatalf("updated preferences: %+v", updated)
	}
	call("PUT", owner, saved, 409)
	if result := call("GET", owner, mailboxPreferences{}, 200); result != updated {
		t.Fatalf("conflicting update changed preferences: %+v", result)
	}
}
