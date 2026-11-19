package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScheduledDelivery(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { testScheduledDelivery(t, "sqlite", filepath.Join(t.TempDir(), "mail.db")) })
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testScheduledDelivery(t, "mysql", dsn) })
	}
}

func testScheduledDelivery(t *testing.T, driver, source string) {
	const sender, recipient, alias = "sender@scheduled.harbor.test", "owner@scheduled.harbor.test", "alias@scheduled.harbor.test"
	a := &app{domain: "harbor.test", accounts: map[string][]byte{sender: nil, recipient: nil}, aliases: map[string]string{alias: recipient}}
	open := func() {
		t.Helper()
		var err error
		a.db, err = openDB(driver, source)
		if err != nil {
			t.Fatal(err)
		}
	}
	open()
	t.Cleanup(func() { _ = a.db.Close() })
	ctx := t.Context()
	send := func(timeParam string, to string, want int) int64 {
		t.Helper()
		body := fmt.Sprintf(`{"to":%q,"subject":"Scheduled mail","body":"test body"}`, to)
		r := httptest.NewRequest(http.MethodPost, "/api/letters?sendAt="+timeParam, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		a.send(w, r, sender)
		if w.Code != want {
			t.Fatalf("schedule: got HTTP %d, want %d (%s)", w.Code, want, w.Body)
		}
		if want != http.StatusAccepted {
			return 0
		}
		var payload map[string]int64
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil || payload["queued"] < 1 {
			t.Fatalf("invalid schedule response: %v (%s)", err, w.Body)
		}
		return payload["queued"]
	}
	for _, invalid := range []string{"", "%ZZ", "1", "NaN", "999999999999999"} {
		send(invalid, alias, http.StatusBadRequest)
	}
	planned := time.Now().Add(time.Hour).Unix()
	param := fmt.Sprint(planned)
	send(param, "external@example.test", http.StatusUnprocessableEntity)
	id := send(param, alias, http.StatusAccepted)
	if job, err := a.claimOutbox(); job != nil || err != nil {
		t.Fatalf("future job was claimable: %+v (%v)", job, err)
	}
	if _, err := a.restoreScheduled(ctx, recipient, id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign cancellation: %v", err)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	open()
	advance := func(id int64) {
		t.Helper()
		if _, err := a.db.Exec(`UPDATE outbox SET due=0 WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(id int64) *outboxJob {
		t.Helper()
		job, err := a.claimOutbox()
		if err != nil || job == nil || job.message.ID != id || job.sendAt != planned {
			t.Fatalf("schedule %d did not survive claim: %+v (%v)", id, job, err)
		}
		return job
	}
	advance(id)
	job := claim(id)
	if _, err := a.restoreScheduled(ctx, sender, id); !errors.Is(err, errScheduleStarted) {
		t.Fatalf("cancel after claim: %v", err)
	}
	for range 2 {
		if err := a.processOutbox(job, nil); err != nil {
			t.Fatal(err)
		}
	}
	var copies int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM letters WHERE recipient=? AND stamp=? AND
		((owner=? AND folder='sent') OR (owner=? AND folder='inbox'))`, alias, planned, sender, recipient).Scan(&copies); err != nil || copies != 2 {
		t.Fatalf("unexpected local copies: %d (%v)", copies, err)
	}

	m := letter{From: sender, To: alias, Subject: "Cancel with file", Body: "message", Date: planned}
	attachment := []byte{0, 17, 128, 255}
	raw, err := encodeLetter(m, a.domain, []uploadedAttachment{{name: "fixture.bin", data: attachment}})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, err := a.scheduleOutgoing(ctx, m, raw, recipient, sendPlan{at: planned})
	if err != nil {
		t.Fatal(err)
	}
	id = scheduled.id
	draft, err := a.restoreScheduled(ctx, sender, id)
	if err != nil {
		t.Fatal(err)
	}
	var revision int
	if err := a.db.QueryRow(`SELECT revision FROM web_drafts WHERE letter_id=?`, draft).Scan(&revision); err != nil || revision != 1 {
		t.Fatalf("restored draft revision: %d (%v)", revision, err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/drafts/id/send?sendAt="+param, nil)
	r.SetPathValue("id", fmt.Sprint(draft))
	r.Header.Set("If-Match", "1")
	w := httptest.NewRecorder()
	a.sendDraft(w, r, sender)
	var payload map[string]int64
	if err := json.Unmarshal(w.Body.Bytes(), &payload); w.Code != 202 || err != nil || payload["sendAt"] != planned {
		t.Fatalf("reschedule draft: HTTP %d (%v)", w.Code, err)
	}
	id = payload["queued"]
	advance(id)
	job = claim(id)
	var file []byte
	visitAttachments(job.raw, func(_ attachmentInfo, part io.Reader) bool {
		file, err = io.ReadAll(part)
		return false
	})
	if err != nil || !bytes.Equal(file, attachment) || job.message.Subject != m.Subject {
		t.Fatalf("rescheduling changed draft or attachment bytes: %v", err)
	}
	if err := a.processOutbox(job, nil); err != nil {
		t.Fatal(err)
	}

	a.relay = "relay.harbor.test:587"
	external := send(param, "external@example.test", http.StatusAccepted)
	advance(external)
	a.relay = ""
	if job, err := a.claimOutbox(); job != nil || err != nil {
		t.Fatalf("external job claimed while relay was disabled: %+v (%v)", job, err)
	}
	a.relay = "relay.harbor.test:587"
	job = claim(external)
	if err := a.processOutbox(job, testRelay(t, a, true, 250)); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	if err := a.db.QueryRow(`SELECT state,reason FROM outbox WHERE id=?`, external).Scan(&state, &reason); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("external delivery remained in state %q: %q (%v)", state, reason, err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM outbox_schedule WHERE outbox_id=?`, external).Scan(&copies); err != nil || copies != 0 {
		t.Fatalf("external schedule metadata leaked after relay acceptance: %d (%v)", copies, err)
	}
}
