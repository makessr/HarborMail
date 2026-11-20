package server

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestConversationBackfillsLegacyMailAndTraversesReplies(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testConversationBackfill(t, "sqlite", filepath.Join(t.TempDir(), "mail.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testConversationBackfill(t, "mysql", dsn) })
	}
}

func testConversationBackfill(t *testing.T, driver, dsn string) {
	t.Helper()
	const owner = "thread-reader@harbor.test"
	db, err := openDB(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, dbDriver: driver}
	fixtures := []struct{ mailbox, folder, headers string }{
		{owner, "sent", "Message-ID: <re2@harbor.test>\r\nIn-Reply-To: <re1@harbor.test>"},
		{owner, "archive", "Message-ID: <root@harbor.test>\r\nReply-To: Replies <replies@harbor.test>"},
		{owner, "inbox", "Message-ID: <re1@harbor.test>\r\nIn-Reply-To: <root@harbor.test>"},
		{owner, "inbox", "Message-ID: <separate@harbor.test>"},
		{owner, "junk", "Message-ID: <excluded@harbor.test>\r\nIn-Reply-To: <root@harbor.test>"},
		{"thread-another@harbor.test", "inbox", "Message-ID: <foreign@harbor.test>\r\nIn-Reply-To: <root@harbor.test>"},
	}
	var fixtureIDs []int64
	defer func() {
		for _, id := range fixtureIDs {
			_, _ = db.Exec("DELETE FROM letters WHERE id=?", id)
		}
	}()
	preview := strings.Repeat("邮", 200)
	for _, fixture := range fixtures {
		// Simulate a row written by a release with no thread index.
		raw := []byte(fixture.headers + "\r\nSubject: Same subject\r\n\r\nBody")
		result, err := db.Exec(`INSERT INTO letters (owner,sender,recipient,subject,body,stamp,folder,origin,raw)
			VALUES (?,'sender@harbor.test',?,'Same subject',?,1,?,'inbox',?)`,
			fixture.mailbox, fixture.mailbox, preview+" truncated", fixture.folder, raw)
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		fixtureIDs = append(fixtureIDs, id)
	}
	ctx := context.Background()
	for range 2 {
		if err := a.backfillThreads(ctx); err != nil {
			t.Fatal(err)
		}
	}
	items, err := a.conversation(ctx, owner, fixtureIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, item := range items {
		ids = append(ids, item.ID)
		if item.Preview != preview {
			t.Fatalf("incorrect thread preview length: %d", len([]rune(item.Preview)))
		}
	}
	if !slices.Equal(ids, fixtureIDs[:3]) {
		t.Fatalf("conversation contains %v", ids)
	}
	headers, err := prepareReply(ctx, a.db, owner, fixtureIDs[2])
	if err != nil || !slices.Equal(headers.parents, []string{"re1@harbor.test"}) ||
		!slices.Equal(headers.references, []string{"root@harbor.test", "re1@harbor.test"}) {
		t.Fatalf("wrong reply headers: %+v, %v", headers, err)
	}
	if _, err := prepareReply(ctx, a.db, owner, fixtureIDs[5]); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign reply source: %v", err)
	}
	raw, err := a.rawLetter(owner, fixtureIDs[1])
	if err != nil || replyAddress(raw, "sender@harbor.test") != "replies@harbor.test" {
		t.Fatalf("Reply-To address not selected: %v", err)
	}
}
