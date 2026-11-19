package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMetricsCountUnresolvedMessages(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a := &app{db: db}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Unix()
	exec(`INSERT INTO outbox (id,sender,recipient,subject,body,stamp,raw,state,due) VALUES
		(1,'sender@harbor.test','a@example.test','','',0,X'00','scheduled',?),
		(2,'sender@harbor.test','a@example.test','','',0,X'00','queued',?),
		(3,'sender@harbor.test','a@example.test','','',0,X'00','uncertain',0),
		(4,'sender@harbor.test','a@example.test','','',0,X'00','uncertain',0)`, now+3600, now-400)
	exec(`INSERT INTO outbox_recipient_progress (outbox_id,sender,recipient,state,attempts,due,smtp_code) VALUES
		(2,'sender@harbor.test','a@example.test','uncertain',1,0,0),
		(2,'sender@harbor.test','b@example.test','uncertain',1,0,0),
		(3,'sender@harbor.test','a@example.test','uncertain',1,0,0),
		(5,'sender@harbor.test','a@example.test','uncertain',1,0,0)`)
	exec(`INSERT INTO spam_feedback (letter_id,owner,sample_class,revision,state,due)
		VALUES (1,'sender@harbor.test','spam',1,'failed',0)`)

	counts, err := a.operationalCounts(t.Context(), now)
	if want := (operationalCounts{OldestDueSeconds: 400, Uncertain: 3, FailedTraining: 1}); err != nil || counts != want {
		t.Fatalf("counts = %+v, err = %v, want %+v", counts, err, want)
	}

	exec(`UPDATE outbox SET state='failed' WHERE state='uncertain' OR id=2`)
	exec(`UPDATE outbox_recipient_progress SET state='accepted' WHERE outbox_id IN (2,3)`)
	exec(`UPDATE spam_feedback SET state='queued'`)
	result := httptest.NewRecorder()
	a.metrics(result, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if result.Code != http.StatusOK {
		t.Fatalf("metrics status = %d: %s", result.Code, result.Body)
	}
	for _, metric := range []string{
		"harbor_outbox_oldest_due_seconds 0\n",
		"harbor_outbox_uncertain_messages 0\n",
		"harbor_spam_training_failed_samples 0\n",
	} {
		if !strings.Contains(result.Body.String(), metric) {
			t.Errorf("metrics missing %q: %s", metric, result.Body)
		}
	}
}
