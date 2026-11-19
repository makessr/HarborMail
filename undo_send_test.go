package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUndoSend(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { testUndoSend(t, "sqlite", filepath.Join(t.TempDir(), "mail.db")) })
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testUndoSend(t, "mysql", dsn) })
	}
}

func testUndoSend(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sender := token()[:16] + "@undo.harbor.test"
	recipient := "recipient-" + sender
	a := &app{db: db, domain: "harbor.test", accounts: map[string][]byte{sender: nil, recipient: nil}}
	ctx := t.Context()
	send := func(query, to string, want int) map[string]int64 {
		t.Helper()
		body := fmt.Sprintf(`{"to":%q,"subject":"Undo fixture","body":"test body"}`, to)
		r := httptest.NewRequest("POST", "/api/letters?"+query, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		a.send(w, r, sender)
		if w.Code != want {
			t.Fatalf("queue: HTTP %d, want %d (%s)", w.Code, want, w.Body)
		}
		var result map[string]int64
		if w.Code == 202 {
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result["queued"] < 1 ||
				result["undoRemainingMs"] <= 0 || result["sendAt"] <= time.Now().Unix() {
				t.Fatalf("undo response %s (%v)", w.Body, err)
			}
		}
		return result
	}
	for _, query := range []string{"undoSeconds=0", "undoSeconds=99", "undoSeconds=abc", "undoSeconds=5&undoSeconds=10", "undoSeconds=5&sendAt=2000000000"} {
		send(query, recipient, 400)
	}
	send("undoSeconds=30", "external@example.test", 422)
	first := send("undoSeconds=30", recipient, 202)["queued"]
	if job, err := a.claimOutbox(); job != nil || err != nil {
		t.Fatalf("undo job dispatch before the deadline: %+v (%v)", job, err)
	}
	if _, err := a.restoreScheduled(ctx, recipient, first); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("another account canceled the mail: %v", err)
	}
	draft, err := a.restoreScheduled(ctx, sender, first)
	if err != nil || draft < 1 {
		t.Fatalf("undo did not restore draft: %d (%v)", draft, err)
	}
	if _, err := a.restoreScheduled(ctx, sender, first); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("repeated undo created a draft: %v", err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/drafts/id/send?undoSeconds=30", nil)
	r.SetPathValue("id", fmt.Sprint(draft))
	r.Header.Set("If-Match", "1")
	a.sendDraft(w, r, sender)
	var pending map[string]int64
	if err := json.Unmarshal(w.Body.Bytes(), &pending); err != nil || w.Code != 202 || pending["undoRemainingMs"] <= 0 {
		t.Fatalf("delayed send from draft: HTTP %d %s (%v)", w.Code, w.Body, err)
	}
	id := pending["queued"]
	if _, err := db.Exec(`UPDATE outbox SET due=0 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.restoreScheduled(ctx, sender, id); !errors.Is(err, errScheduleStarted) {
		t.Fatalf("expired cancel before claim: %v", err)
	}
	job, err := a.claimOutbox()
	if err != nil || job == nil || job.message.ID != id {
		t.Fatalf("did not claim expired undo job: %+v (%v)", job, err)
	}
	for range 2 {
		if err := a.processOutbox(job, nil); err != nil {
			t.Fatal(err)
		}
	}
	var copies int
	if err := db.QueryRow(`SELECT COUNT(*) FROM letters WHERE owner=? AND folder='inbox' AND subject='Undo fixture'`,
		recipient).Scan(&copies); err != nil || copies != 1 {
		t.Fatalf("local copies after deadline: %d (%v)", copies, err)
	}
}
