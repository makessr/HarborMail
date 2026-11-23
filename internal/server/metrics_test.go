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
	exec(`INSERT INTO outbox (id,sender,recipient,subject,body,stamp,raw,state,due,lease) VALUES
		(6,'sender@harbor.test','a@example.test','','',0,X'00','preparing',?,?),
		(7,'sender@harbor.test','a@example.test','','',0,X'00','sending',?,?),
		(8,'sender@harbor.test','a@example.test','','',0,X'00','preparing',?,?)`,
		now-1100, now-600, now-1200, now-500, now-1300, now+60)
	exec(`INSERT INTO spam_feedback (letter_id,owner,sample_class,revision,state,due)
		VALUES (1,'sender@harbor.test','spam',1,'failed',0)`)

	counts, err := a.operationalCounts(t.Context(), now)
	if want := (operationalCounts{OldestDueSeconds: 600, Uncertain: 3, FailedTraining: 1}); err != nil || counts != want {
		t.Fatalf("counts = %+v, err = %v, want %+v", counts, err, want)
	}

	exec(`UPDATE outbox SET state='failed' WHERE state='uncertain' OR id IN (2,6,7)`)
	exec(`UPDATE outbox_recipient_progress SET state='accepted' WHERE outbox_id IN (2,3)`)
	exec(`UPDATE spam_feedback SET state='queued'`)
	if !a.submissions.beginMessage(mailLimit) || a.submissions.beginMessage(defaultInboundLimits.bytes) {
		t.Fatal("expected one Submission reservation and one deferral")
	}
	defer a.submissions.endMessage(mailLimit)
	result := httptest.NewRecorder()
	a.metrics(result, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if result.Code != http.StatusOK {
		t.Fatalf("metrics status = %d: %s", result.Code, result.Body)
	}
	for _, metric := range []string{
		"harbor_outbox_oldest_due_seconds 0\n",
		"harbor_outbox_paused 0\n",
		"harbor_outbox_uncertain_messages 0\n",
		"harbor_spam_training_failed_samples 0\n",
		"harbor_dkim_active_domains 0\n",
		"harbor_dkim_unusable_domains 0\n",
		"harbor_submission_active_messages 1\n",
		"harbor_submission_reserved_bytes 10485760\n",
		"harbor_submission_capacity_deferred_total 1\n",
		"harbor_inbound_capacity_deferred_total 0\n",
	} {
		if !strings.Contains(result.Body.String(), metric) {
			t.Errorf("metrics missing %q: %s", metric, result.Body)
		}
	}
	for _, paused := range []*app{{db: db, pauseOutbox: true}, {db: db, recoveryMode: true}} {
		result := httptest.NewRecorder()
		paused.metrics(result, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if !strings.Contains(result.Body.String(), "harbor_outbox_paused 1\n") {
			t.Fatalf("paused state missing in metrics: %s", result.Body)
		}
	}
}

func TestMetricsDKIMDetectsUnusableKeysPerInstance(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	correct := testDKIMCipher(t)
	a := &app{db: db, dkimCipher: correct}
	for _, domain := range []string{"healthy.test", "damaged.test", "staged.test"} {
		installTestDKIMKey(t, a, domain)
	}
	if _, err := db.Exec(`UPDATE domain_dkim_keys SET public_key='different public key' WHERE domain='damaged.test'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE domain_dkim_keys SET slot='pending' WHERE domain='staged.test'`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARBOR_DKIM_KEY", strings.Repeat("70", 32))
	wrong, err := loadDKIMCipher()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		source *app
		metric string
	}{
		{"matching secret", a, "harbor_dkim_unusable_domains 1\n"},
		{"wrong secret", &app{db: db, dkimCipher: wrong}, "harbor_dkim_unusable_domains 2\n"},
		{"missing secret", &app{db: db}, "harbor_dkim_unusable_domains 2\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := httptest.NewRecorder()
			tc.source.metrics(result, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			body := result.Body.String()
			if result.Code != http.StatusOK || !strings.Contains(body, "harbor_dkim_active_domains 2\n") || !strings.Contains(body, tc.metric) {
				t.Fatalf("unexpected metric response: %d %s", result.Code, body)
			}
			if strings.Contains(body, ".test") || strings.Contains(body, "different public key") || strings.Contains(body, "harbor-") {
				t.Fatal("metrics must not expose domains, selectors or key data")
			}
		})
	}
}
