package server

import (
	"database/sql"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyDeliveryTimelineKeepsUnknownRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`CREATE TABLE delivery_events (id INTEGER PRIMARY KEY AUTOINCREMENT, outbox_id INTEGER NOT NULL,
		owner TEXT NOT NULL, recipient TEXT NOT NULL, state TEXT NOT NULL, stamp INTEGER NOT NULL,
		due INTEGER NOT NULL, smtp_code INTEGER NOT NULL);
		INSERT INTO delivery_events(outbox_id,owner,recipient,state,stamp,due,smtp_code)
		VALUES(1,'sender@harbor.test','recipient@example.test','sent',1,0,250);`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	db, err := openDB("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	events, err := readDeliveryTimeline(t.Context(), db, "sender@harbor.test", 1)
	if err != nil || len(events) != 1 || events[0].State != "sent" || events[0].Route != "" || events[0].Code != 250 {
		t.Fatalf("legacy event was not preserved: %+v (%v)", events, err)
	}
}

func TestMigrationPreflightRejectsInFlightRecipients(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail.db")
	db, err := openDB("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := enqueueLetter(db, letter{From: "sender@harbor.test", To: "one@example.test"}, []byte("Synthetic mail"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO outbox_recipient_progress(outbox_id,sender,recipient,state,attempts,due,smtp_code)
		VALUES(?,'sender@harbor.test','one@example.test','sending',1,0,0)`, id); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARBOR_MIGRATION_MYSQL_DSN", "")
	t.Setenv("HARBOR_MIGRATION_MYSQL_DSN_FILE", "")
	preflight := func() error {
		return runSQLiteMigration([]string{"--sqlite", path}, io.Discard)
	}
	if err := preflight(); err == nil || !strings.Contains(err.Error(), "in-flight recipient") {
		t.Fatalf("expected preflight to reject unfinished recipient confirmation, got %v", err)
	}
	if _, err := db.Exec(`UPDATE outbox_recipient_progress SET state='uncertain' WHERE outbox_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := preflight(); err != nil {
		t.Fatalf("a frozen recipient can be migrated: %v", err)
	}
}
