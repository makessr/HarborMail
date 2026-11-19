package server

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

func TestRecoverRecipientDeliveryAfterInterruption(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testRecoverRecipientDelivery(t, "sqlite", filepath.Join(t.TempDir(), "mail.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testRecoverRecipientDelivery(t, "mysql", dsn) })
	}
}

func testRecoverRecipientDelivery(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, dbDriver: driver, domain: "harbor.test"}
	sender := token()[:16] + "@harbor.test"
	retryAt := time.Now().Add(time.Hour).Unix()
	for _, otherState := range []string{"accepted", "queued"} {
		m := letter{From: sender, To: "one@example.test", Subject: "interrupted", Date: time.Now().Unix()}
		id, err := a.enqueue(m, []byte("Subject: interrupted\r\n\r\nSynthetic body"))
		if err != nil {
			t.Fatal(err)
		}
		claim := token()
		if _, err := db.Exec(`UPDATE outbox SET state='sending',attempts=1,claim=?,lease=0 WHERE id=?`, claim, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO outbox_recipient_progress(outbox_id,sender,recipient,state,attempts,due,smtp_code)
			VALUES(?,?,'one@example.test','sending',1,0,0),(?,?,'two@example.test',?,1,?,0)`,
			id, sender, id, sender, otherState, retryAt); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := a.recoverOutbox(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		want, wantDue := "uncertain", int64(0)
		if otherState == "queued" {
			want, wantDue = "queued", retryAt
		}
		var state, storedClaim string
		var due, lease int64
		if err := db.QueryRow(`SELECT state,due,claim,lease FROM outbox WHERE id=?`, id).
			Scan(&state, &due, &storedClaim, &lease); err != nil || state != want || due != wantDue || storedClaim != "" || lease != 0 {
			t.Fatalf("recovery with %s peer: state=%s due=%d claim=%q lease=%d (%v)", otherState, state, due, storedClaim, lease, err)
		}
		items, err := readRecipientProgress(t.Context(), db, sender, id)
		if err != nil || len(items) != 2 || items[0].State != "uncertain" || items[0].Attempts != 1 || items[1].State != otherState {
			t.Fatalf("recipient results after recovery: %+v (%v)", items, err)
		}
	}
	m := letter{From: sender, To: "one@example.test", Subject: "already advanced", Date: time.Now().Unix()}
	id, err := a.enqueue(m, []byte("Subject: already advanced\r\n\r\nSynthetic body"))
	if err != nil {
		t.Fatal(err)
	}
	m.ID = id
	claim := token()
	if _, err := db.Exec(`UPDATE outbox SET state='preparing',claim=?,lease=? WHERE id=?`, claim, retryAt, id); err != nil {
		t.Fatal(err)
	}
	// The recovery scan saw an expired sending job before a worker recorded
	// its reply and advanced the parent with the same claim.
	if err := a.recoverOutboxJob(&outboxJob{message: m, claim: claim}); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := db.QueryRow(`SELECT state FROM outbox WHERE id=?`, id).Scan(&state); err != nil || state != "preparing" {
		t.Fatalf("stale scan changed an active task: state=%s (%v)", state, err)
	}
	var notices int
	if err := db.QueryRow(`SELECT COUNT(*) FROM letters WHERE owner=? AND sender='postmaster@harbor.test'`, sender).
		Scan(&notices); err != nil || notices != 1 {
		t.Fatalf("expected one notice for the fully classified job: %d (%v)", notices, err)
	}

	for _, tc := range []struct {
		recipientState string
		jobState       string
		want           string
	}{
		{"uncertain", "queued", "uncertain"}, // Older workers requeued recovered jobs.
		{"accepted", "preparing", "sent"},
		{"failed", "preparing", "failed"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			m := letter{From: sender, To: "one@example.test", Subject: "interrupted-" + tc.want, Date: time.Now().Unix()}
			stuck, err := a.enqueue(m, []byte("Subject: "+m.Subject+"\r\n\r\nSynthetic body"))
			if err != nil {
				t.Fatal(err)
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			envelope := webEnvelope{Targets: []webTarget{{Address: "one@example.test"}, {Address: "two@example.test"}}}
			if err := saveWebEnvelope(t.Context(), tx, stuck, envelope); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(`UPDATE outbox SET state=?,lease=0 WHERE id=?`, tc.jobState, stuck); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(`INSERT INTO outbox_recipient_progress(outbox_id,sender,recipient,state,attempts,due,smtp_code)
				VALUES(?,?,'one@example.test',?,1,0,0),(?,?,'two@example.test','accepted',1,0,250)`,
				stuck, sender, tc.recipientState, stuck, sender); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			job, err := a.claimOutbox()
			if err != nil || job == nil || job.message.ID != stuck {
				t.Fatalf("no claim for task %d with results recorded: %+v (%v)", stuck, job, err)
			}
			if err := a.processOutbox(job, nil); err != nil {
				t.Fatal(err)
			}
			err = db.QueryRow(`SELECT state FROM outbox WHERE id=?`, stuck).Scan(&state)
			if tc.want == "sent" {
				var archived int
				archiveErr := db.QueryRow(`SELECT COUNT(*) FROM letters WHERE owner=? AND folder='sent' AND subject=?`,
					sender, m.Subject).Scan(&archived)
				if !errors.Is(err, sql.ErrNoRows) || archiveErr != nil || archived != 1 {
					t.Fatalf("sent task: queue error=%v, archived=%d (%v)", err, archived, archiveErr)
				}
			} else if err != nil || state != tc.want {
				t.Fatalf("task state=%s, want %s (%v)", state, tc.want, err)
			}
		})
	}
}

func TestOutboxRetriesOnlyTransientRecipient(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, domain: "harbor.test", relay: domainRelay{Address: "relay.harbor.test:587"}}
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
