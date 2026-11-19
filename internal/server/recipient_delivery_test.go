package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

func TestOutboxRetriesOnlyTransientRecipient(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, domain: "harbor.test", relay: "relay.harbor.test:587"}
	const sender = "sender@harbor.test"
	item := letter{From: sender, To: "a@example.test", Subject: "separate delivery", Body: "test body", Date: time.Now().Unix()}
	raw, err := encodeLetter(item, a.domain, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := a.enqueue(item, raw)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	envelope := webEnvelope{Targets: []webTarget{{Address: "a@example.test"}, {Address: "b@example.test"}, {Address: "c@example.test"}}}
	if err := saveWebEnvelope(ctx, tx, id, envelope); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	run := func(codes ...int) {
		t.Helper()
		connectors := make([]relayConnector, 0, len(codes))
		for _, code := range codes {
			connectors = append(connectors, testRelay(t, a, true, code))
		}
		calls := 0
		job, err := a.claimOutbox()
		if err != nil || job == nil || job.message.ID != id {
			t.Fatalf("claim delivery: job=%v err=%v", job, err)
		}
		if err := a.processOutbox(job, func(ctx context.Context) (*smtp.Client, func(), error) {
			if calls >= len(connectors) {
				t.Fatal("already attempted a recipient that should not be retried")
			}
			next := connectors[calls]
			calls++
			return next(ctx)
		}); err != nil || calls != len(connectors) {
			t.Fatalf("deliver tracked recipients: calls=%d err=%v", calls, err)
		}
	}
	run(250, 450, 0)
	check := func(expected ...string) []recipientProgress {
		t.Helper()
		items, err := readRecipientProgress(ctx, db, sender, id)
		if err != nil || len(items) != len(expected) {
			t.Fatalf("tracked results: %+v, err=%v", items, err)
		}
		for index, state := range expected {
			if items[index].State != state {
				t.Fatalf("recipient %d = %+v, want %s", index, items[index], state)
			}
		}
		return items
	}
	check("accepted", "queued", "uncertain")
	if _, err := db.Exec(`UPDATE outbox_recipient_progress SET due=0 WHERE outbox_id=? AND state='queued'`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE outbox SET due=0 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	run(250)
	items := check("accepted", "accepted", "uncertain")
	if items[0].Attempts != 1 || items[1].Attempts != 2 || items[2].Attempts != 1 {
		t.Fatalf("unexpected replay counts: %+v", items)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM outbox WHERE id=?`, id).Scan(&state); err != nil || state != "uncertain" {
		t.Fatalf("parent state = %q, err=%v", state, err)
	}
}
