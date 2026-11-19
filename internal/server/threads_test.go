package server

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

func TestConversationBackfillsLegacyMailAndTraversesReplies(t *testing.T) {
	const owner = "reader@harbor.test"
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db}
	fixtures := []struct{ mailbox, folder, headers string }{
		{owner, "sent", "Message-ID: <re2@harbor.test>\r\nIn-Reply-To: <re1@harbor.test>"},
		{owner, "archive", "Message-ID: <root@harbor.test>\r\nReply-To: Replies <replies@harbor.test>"},
		{owner, "inbox", "Message-ID: <re1@harbor.test>\r\nIn-Reply-To: <root@harbor.test>"},
		{owner, "inbox", "Message-ID: <separate@harbor.test>"},
		{owner, "junk", "Message-ID: <excluded@harbor.test>\r\nIn-Reply-To: <root@harbor.test>"},
		{"another@harbor.test", "inbox", "Message-ID: <foreign@harbor.test>\r\nIn-Reply-To: <root@harbor.test>"},
	}
	for _, fixture := range fixtures {
		// Simulate a row written by a release with no thread index.
		raw := []byte(fixture.headers + "\r\nSubject: Same subject\r\n\r\nBody")
		if _, err := db.Exec(`INSERT INTO letters (owner,sender,recipient,subject,body,stamp,folder,origin,raw)
			VALUES (?,'sender@harbor.test',?,'Same subject','',1,?,'inbox',?)`,
			fixture.mailbox, fixture.mailbox, fixture.folder, raw); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	for range 2 {
		if err := a.backfillThreads(ctx); err != nil {
			t.Fatal(err)
		}
	}
	items, err := a.conversation(ctx, owner, 1)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	if !slices.Equal(ids, []int64{1, 2, 3}) {
		t.Fatalf("conversation contains %v", ids)
	}
	headers, err := prepareReply(ctx, a.db, owner, 3)
	if err != nil || !slices.Equal(headers.parents, []string{"re1@harbor.test"}) ||
		!slices.Equal(headers.references, []string{"root@harbor.test", "re1@harbor.test"}) {
		t.Fatalf("wrong reply headers: %+v, %v", headers, err)
	}
	if _, err := prepareReply(ctx, a.db, owner, 6); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign reply source: %v", err)
	}
	raw, err := a.rawLetter(owner, 2)
	if err != nil || replyAddress(raw, "sender@harbor.test") != "replies@harbor.test" {
		t.Fatalf("Reply-To address not selected: %v", err)
	}
}
