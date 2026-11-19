package server

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

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
