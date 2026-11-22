package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	call := func(method, actor string, input any, status int) mailboxPreferences {
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
		{Theme: "dark", Density: "compact", ReadMode: "manual", SenderDisplayName: "Jason\r\nBcc: invalid@example.test"},
		{Theme: "dark", Density: "compact", ReadMode: "manual", SenderDisplayName: strings.Repeat("王", 81)},
	} {
		call("PUT", owner, invalid, 400)
	}
	subscription := a.events.watch(owner)
	selected := mailboxPreferences{Theme: "dark", Density: "compact", ReadMode: "manual", UndoSeconds: 10, SenderDisplayName: "王 Jason"}
	withSpaces := selected
	withSpaces.SenderDisplayName = "  " + withSpaces.SenderDisplayName + "  "
	saved := call("PUT", owner, withSpaces, 200)
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
	legacy := json.RawMessage(`{"revision":2,"theme":"light","density":"compact","readMode":"manual","undoSeconds":10}`)
	preserved := call("PUT", owner, legacy, 200)
	if preserved.SenderDisplayName != selected.SenderDisplayName {
		t.Fatalf("old client cleared the display name: %+v", preserved)
	}
	preserved.SenderDisplayName = ""
	if cleared := call("PUT", owner, preserved, 200); cleared.SenderDisplayName != "" {
		t.Fatalf("failed to clear the display name: %+v", cleared)
	}
}

func TestMailboxPreferencesLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	oldDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = oldDB.Exec(`CREATE TABLE mailbox_preferences (
		owner TEXT NOT NULL UNIQUE, revision INTEGER NOT NULL, theme TEXT NOT NULL, density TEXT NOT NULL, read_mode TEXT NOT NULL
	);
	INSERT INTO mailbox_preferences VALUES ('old@harbor.test',4,'dark','compact','manual');`)
	oldDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	db, err := openDB("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name, theme string
	var revision int
	err = db.QueryRow(`SELECT sender_display_name,theme,revision FROM mailbox_preferences WHERE owner='old@harbor.test'`).
		Scan(&name, &theme, &revision)
	if err != nil || name != "" || theme != "dark" || revision != 4 {
		t.Fatalf("legacy preferences were not preserved: %q, %q, %d, %v", name, theme, revision, err)
	}
}
