package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestMailSnoozes(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { testMailSnoozes(t, "sqlite", filepath.Join(t.TempDir(), "mail.db")) })
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testMailSnoozes(t, "mysql", dsn) })
	}
}

func testMailSnoozes(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db}
	ctx := t.Context()
	owner := token()[:16] + "@snooze.harbor.test"
	raw := []byte("Subject: snooze fixture\r\n\r\noriginal\r\n")
	const stamp = 1724000000
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	id, err := insertLetter(tx, owner, letter{From: "sender@harbor.test", To: owner, Date: stamp, Star: true}, "inbox", "inbox", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	call := func(actor string, change snoozeChange, status int) {
		t.Helper()
		payload, err := json.Marshal(change)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("PUT", "/api/letters/snooze", bytes.NewReader(payload))
		r.SetPathValue("id", strconv.FormatInt(id, 10))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		a.setSnooze(w, r, actor)
		if w.Code != status {
			t.Fatalf("snooze HTTP %d, want %d: %s", w.Code, status, w.Body)
		}
	}
	expectList := func(folder string, count int, resumeAt int64) {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/letters?folder="+folder, nil)
		w := httptest.NewRecorder()
		a.list(w, r, owner)
		var result struct{ Letters []letter }
		if err := json.Unmarshal(w.Body.Bytes(), &result); w.Code != 200 || err != nil || len(result.Letters) != count {
			t.Fatalf("folder %s: HTTP %d %s (%v)", folder, w.Code, w.Body, err)
		}
		if count == 1 {
			item := result.Letters[0]
			if item.ID != id || item.SnoozeUntil != resumeAt || item.Read || !item.Star || item.Date != stamp {
				t.Fatalf("folder %s: %+v", folder, item)
			}
		}
	}
	messages, _, _, err := a.imapSnapshot(owner, "inbox")
	if err != nil || len(messages) != 1 {
		t.Fatalf("IMAP snapshot: %v (%v)", messages, err)
	}
	oldUID := messages[0].Uid
	now := time.Now().Unix()
	first, later := now+60, now+120
	call("other-"+owner, snoozeChange{ResumeAt: first}, 404)
	call(owner, snoozeChange{ResumeAt: now - 1}, 400)
	call(owner, snoozeChange{ResumeAt: first}, 204)
	expectList("inbox", 0, 0)
	expectList("snoozed", 1, first)
	expectList("starred", 1, first)
	messages, _, _, err = a.imapSnapshot(owner, "inbox")
	if err != nil || len(messages) != 0 {
		t.Fatalf("snoozed mail in IMAP inbox: %v (%v)", messages, err)
	}
	call(owner, snoozeChange{ResumeAt: later, PreviousAt: first}, 204)
	call(owner, snoozeChange{PreviousAt: first}, 409)
	if err := a.restoreDueSnoozes(ctx, first); err != nil {
		t.Fatal(err)
	}
	expectList("snoozed", 1, later)
	ready := a.events.watch(owner)
	if err := a.restoreDueSnoozes(ctx, later); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	default:
		t.Fatal("restoring a snooze did not notify its owner")
	}
	expectList("inbox", 1, 0)
	expectList("snoozed", 0, 0)
	messages, _, _, err = a.imapSnapshot(owner, "inbox")
	if err != nil || len(messages) != 1 || messages[0].Uid <= oldUID {
		t.Fatalf("restored mailbox UID: %v (%v)", messages, err)
	}
	if got, err := a.rawLetter(owner, id); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("restored MIME: %q (%v)", got, err)
	}
	call(owner, snoozeChange{ResumeAt: later}, 204)
	call(owner, snoozeChange{PreviousAt: later}, 204)
	expectList("inbox", 1, 0)
	call(owner, snoozeChange{ResumeAt: later}, 204)
	for _, folder := range []string{"trash", "restore"} {
		if err := a.changeLetters(ctx, owner, []int64{id}, letterChange{Folder: &folder}); err != nil {
			t.Fatal(err)
		}
	}
	expectList("inbox", 1, 0)
	expectList("snoozed", 0, 0)
}
