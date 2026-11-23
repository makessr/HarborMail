package server

import (
	"bufio"
	"bytes"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPOP3QuitLeavesMovedDraftUntouched(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testPOP3QuitLeavesMovedDraftUntouched(t, "sqlite", filepath.Join(t.TempDir(), "mail.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) {
			testPOP3QuitLeavesMovedDraftUntouched(t, "mysql", dsn)
		})
	}
}

func testPOP3QuitLeavesMovedDraftUntouched(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := token()[:12] + "@harbor.test"
	a := &app{db: db, dbDriver: driver}
	insert := func() int64 {
		t.Helper()
		result, err := db.Exec(`INSERT INTO letters(owner,sender,recipient,subject,body,stamp,folder,origin,raw)
			VALUES(?,?,?,'subject','',1,'inbox','inbox',?)`, owner, owner, owner, []byte("Subject: test\r\n\r\nBody"))
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	moved, inbox := insert(), insert()
	messages, err := a.popSnapshot(owner)
	if err != nil {
		t.Fatal(err)
	}
	var responses bytes.Buffer
	s := &popSession{server: &popServer{app: a}, owner: owner, messages: messages,
		writer: textproto.NewWriter(bufio.NewWriter(&responses))}
	for _, number := range []string{"1", "2"} {
		if done, err := s.command("DELE", number); done || err != nil {
			t.Fatalf("DELE %s: done=%t err=%v", number, done, err)
		}
	}
	// Model the other client moving a draft with a saved revision back to Drafts.
	if _, err := db.Exec(`UPDATE letters SET folder='drafts' WHERE id=?`, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO web_drafts(letter_id,revision) VALUES(?,1)`, moved); err != nil {
		t.Fatal(err)
	}
	if done, err := s.command("QUIT", ""); !done || err != nil || !strings.Contains(responses.String(), "+OK Goodbye") {
		t.Fatalf("QUIT: done=%t err=%v, responses=%q", done, err, responses.String())
	}
	var folder string
	var revision int
	if err := db.QueryRow(`SELECT l.folder,d.revision FROM letters l JOIN web_drafts d ON d.letter_id=l.id WHERE l.id=?`, moved).
		Scan(&folder, &revision); err != nil || folder != "drafts" || revision != 1 {
		t.Fatalf("moved draft: folder=%q revision=%d err=%v", folder, revision, err)
	}
	if err := db.QueryRow(`SELECT folder FROM letters WHERE id=?`, inbox).Scan(&folder); err != nil || folder != "trash" {
		t.Fatalf("deleted inbox mail: folder=%q err=%v", folder, err)
	}
}
