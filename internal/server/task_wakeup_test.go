package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestTaskWorkersWakeForLocalDeadlines(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const sender, owner = "sender@harbor.test", "owner@harbor.test"
	a := &app{db: db, dbDriver: "sqlite", accounts: map[string][]byte{sender: {}, owner: {}}}
	ctx, stop := context.WithCancel(t.Context())
	outboxDone, snoozesDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(outboxDone)
		a.runOutbox(ctx, nil)
	}()
	go func() {
		defer close(snoozesDone)
		a.runSnoozes(ctx)
	}()
	defer func() {
		stop()
		<-outboxDone
		<-snoozesDone
	}()
	message := letter{From: sender, To: owner, Subject: "New delivery"}
	raw, err := encodeLetter(message, "harbor.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	message.Subject = "Existing"
	id, err := insertLetter(t.Context(), tx, owner, message, "inbox", "inbox", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(2 * time.Second).Unix()
	if err := a.changeSnooze(ctx, owner, id, snoozeChange{ResumeAt: due}); err != nil {
		t.Fatal(err)
	}
	message.Subject = "New delivery"
	if _, err := a.scheduleOutgoing(ctx, message, raw, owner, sendPlan{at: due}); err != nil {
		t.Fatal(err)
	}
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	timeout := time.NewTimer(6 * time.Second)
	defer timeout.Stop()
	for {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM letters WHERE owner=? AND folder='inbox'`, owner).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 2 {
			if time.Now().Unix() < due {
				t.Fatal("a worker completed its task before the due time")
			}
			return
		}
		select {
		case <-tick.C:
		case <-timeout.C:
			t.Fatalf("scheduled delivery or snooze did not finish; inbox count %d", count)
		}
	}
}
