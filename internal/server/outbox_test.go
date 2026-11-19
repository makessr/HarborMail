package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

func TestOutboxExpiresWhenSenderRelayIsRemoved(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testOutboxWithoutRelay(t, "sqlite", filepath.Join(t.TempDir(), "mail.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testOutboxWithoutRelay(t, "mysql", dsn) })
	}
}

func testOutboxWithoutRelay(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, dbDriver: driver, domain: "harbor.test",
		domainRelays: map[string]domainRelay{"other.harbor.test": {Address: "relay.other.harbor.test:587"}}}
	const sender = "sender@harbor.test"
	now := time.Now()
	expired := now.Add(-outboxMaxAge - time.Hour).Unix()
	queue := func(subject string) int64 {
		t.Helper()
		m := letter{From: sender, To: "one@example.test", Subject: subject, Body: "Synthetic body", Date: expired}
		raw, err := encodeLetter(m, a.domain, nil)
		if err != nil {
			t.Fatal(err)
		}
		id, err := a.enqueue(m, raw)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	scheduled := queue("recently due")
	if _, err := db.Exec(`UPDATE outbox SET state='scheduled' WHERE id=?`, scheduled); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO outbox_schedule(outbox_id,send_at,local_owner) VALUES(?,?,'')`,
		scheduled, now.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	legacy := queue("expired")
	multi := queue("partially delivered")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	envelope := webEnvelope{Targets: []webTarget{{Address: "one@example.test"}, {Address: "two@example.test"}}}
	if err := saveWebEnvelope(t.Context(), tx, multi, envelope); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO outbox_recipient_progress(outbox_id,sender,recipient,state,attempts,due,smtp_code)
		VALUES(?,?,'one@example.test','accepted',1,0,250),(?,?,'two@example.test','queued',1,?,450)`,
		multi, sender, multi, sender, now.Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	connect := func(context.Context) (*smtp.Client, func(), error) {
		t.Fatal("the worker opened a relay connection for expired mail")
		return nil, nil, nil
	}
	for _, id := range []int64{legacy, multi} {
		job, err := a.claimOutbox()
		if err != nil || job == nil || job.message.ID != id {
			t.Fatalf("could not claim expired task %d: %+v (%v)", id, job, err)
		}
		if err := a.processOutbox(job, connect); err != nil {
			t.Fatal(err)
		}
		var state string
		if err := db.QueryRow(`SELECT state FROM outbox WHERE id=?`, id).Scan(&state); err != nil || state != "failed" {
			t.Fatalf("expired task %d: state=%q (%v)", id, state, err)
		}
	}
	rows, err := readRecipientProgress(t.Context(), db, sender, multi)
	if err != nil || len(rows) != 2 || rows[0].State != "accepted" || rows[1].State != "failed" {
		t.Fatalf("unexpected recipient results after expiration: %+v (%v)", rows, err)
	}
	if job, err := a.claimOutbox(); job != nil || err != nil {
		t.Fatalf("recently scheduled job should wait for a relay: %+v (%v)", job, err)
	}
	if _, err := a.restoreScheduled(t.Context(), sender, scheduled); err != nil {
		t.Fatalf("clean up the scheduled job before another test enables a relay: %v", err)
	}
}

func TestMixedOutboxDeliversLocallyWithoutRelay(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testMixedOutboxWithoutRelay(t, "sqlite", filepath.Join(t.TempDir(), "mail.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testMixedOutboxWithoutRelay(t, "mysql", dsn) })
	}
}

func testMixedOutboxWithoutRelay(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sender := token()[:16] + "@harbor.test"
	recipient := "recipient-" + sender
	a := &app{db: db, dbDriver: driver, domain: "harbor.test",
		accounts: map[string][]byte{sender: nil, recipient: nil}}
	a.relay.Address = "relay.harbor.test:587"
	r := httptest.NewRequest(http.MethodPost, "/api/letters", strings.NewReader(
		fmt.Sprintf(`{"to":%q,"subject":"Mixed delivery","body":"test body"}`, recipient+",external@example.test")))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.send(w, r, sender)
	var queued struct{ Queued int64 }
	if err := json.Unmarshal(w.Body.Bytes(), &queued); err != nil || w.Code != http.StatusAccepted || queued.Queued == 0 {
		t.Fatalf("queue mixed mail: HTTP %d, %s (%v)", w.Code, w.Body, err)
	}
	a.relay.Address = ""
	claim := func() *outboxJob {
		t.Helper()
		job, err := a.claimOutbox()
		if err != nil || job == nil || job.message.ID != queued.Queued {
			t.Fatalf("could not claim mixed job %d: %+v (%v)", queued.Queued, job, err)
		}
		return job
	}
	// The local leg must finish without attempting SMTP.
	if err := a.processOutbox(claim(), nil); err != nil {
		t.Fatal(err)
	}
	var state string
	var localDone bool
	if err := db.QueryRow(`SELECT o.state,e.local_done FROM outbox o JOIN web_outbox_envelopes e ON e.outbox_id=o.id
		WHERE o.id=? AND o.claim='' AND o.lease=0`, queued.Queued).Scan(&state, &localDone); err != nil || state != "queued" || !localDone {
		t.Fatalf("local progress=%v, external state=%q (%v)", localDone, state, err)
	}
	checkCopies := func() {
		t.Helper()
		for owner, folder := range map[string]string{recipient: "inbox", sender: "sent"} {
			var copies int
			err := db.QueryRow(`SELECT COUNT(*) FROM letters WHERE owner=? AND folder=? AND subject='Mixed delivery'`,
				owner, folder).Scan(&copies)
			if err != nil || copies != 1 {
				t.Fatalf("%s copies for %s = %d (%v)", folder, owner, copies, err)
			}
		}
	}
	checkCopies()
	if job, err := a.claimOutbox(); job != nil || err != nil {
		t.Fatalf("remaining external delivery was claimed with no relay: %+v (%v)", job, err)
	}
	a.relay.Address = "relay.harbor.test:587"
	if err := a.processOutbox(claim(), testRelay(t, a, true, 250)); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT state FROM outbox WHERE id=?`, queued.Queued).Scan(&state); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("mixed job remained in state %q after acceptance: %v", state, err)
	}
	checkCopies()
}

func TestOutboxRelay(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testOutboxRelay(t, "sqlite", filepath.Join(t.TempDir(), "mail.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testOutboxRelay(t, "mysql", dsn) })
	}
}

func testOutboxRelay(t *testing.T, driver, source string) {
	a := &app{domain: "harbor.test", relay: domainRelay{Address: "relay.harbor.test:587"}}
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
	const owner = "sender@outbox-test.harbor.test"
	queue := func(subject string) int64 {
		t.Helper()
		m := letter{From: owner, To: "recipient@example.test", Subject: subject, Body: "test body", Date: time.Now().Unix()}
		raw, err := encodeLetter(m, a.domain, nil)
		if err != nil {
			t.Fatal(err)
		}
		id, err := a.enqueue(m, raw)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	checkState := func(id int64, want string) deliveryStatus {
		t.Helper()
		var status deliveryStatus
		err := a.db.QueryRow(`SELECT state,attempts,due,reason FROM outbox WHERE id=?`, id).
			Scan(&status.State, &status.Attempts, &status.Due, &status.Reason)
		if want == "sent" && errors.Is(err, sql.ErrNoRows) {
			return status
		}
		if err != nil || status.State != want {
			t.Fatalf("job %d = %+v, error = %v, want %q", id, status, err, want)
		}
		return status
	}
	execute := func(id int64, implicit bool, replyCode int) {
		t.Helper()
		job, err := a.claimOutbox()
		if err != nil || job == nil || job.message.ID != id {
			t.Fatalf("could not claim job %d: %v", id, err)
		}
		if err := a.processOutbox(job, testRelay(t, a, implicit, replyCode)); err != nil {
			t.Fatal(err)
		}
	}
	id := queue("retryable")
	execute(id, false, 450)
	status := checkState(id, "queued")
	if status.Attempts != 1 || status.Due < time.Now().Add(29*time.Minute).Unix() {
		t.Fatalf("unexpected retry: %+v", status)
	}
	if _, err := a.db.Exec(`UPDATE outbox SET due=0 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	open()
	execute(id, true, 250)
	checkState(id, "sent")
	var sent int
	if err := a.db.QueryRow(`SELECT count(*) FROM letters WHERE owner=? AND folder='sent' AND subject='retryable'`,
		owner).Scan(&sent); err != nil || sent != 1 {
		t.Fatalf("sent copies = %d, err = %v", sent, err)
	}

	failed := queue("rejected")
	execute(failed, true, 550)
	checkState(failed, "failed")
	uncertain := queue("missing final reply")
	execute(uncertain, true, 0)
	checkState(uncertain, "uncertain")
	if job, err := a.claimOutbox(); err != nil || job != nil {
		t.Fatalf("terminal job was claimable: %v", err)
	}
	var notices int
	if err := a.db.QueryRow(`SELECT count(*) FROM letters WHERE owner=? AND folder='inbox'`, owner).
		Scan(&notices); err != nil || notices != 2 {
		t.Fatalf("inbox notices = %d, err = %v", notices, err)
	}

	authFailed := queue("relay password changed")
	authJob, err := a.claimOutbox()
	if err != nil || authJob == nil || authJob.message.ID != authFailed {
		t.Fatalf("could not claim auth test job %d: %v", authFailed, err)
	}
	if err := a.processOutbox(authJob, func(context.Context) (*smtp.Client, func(), error) {
		return nil, nil, fmt.Errorf("%w: %w", errRelayAuthentication, smtp.ErrAuthFailed)
	}); err != nil {
		t.Fatal(err)
	}
	retry := checkState(authFailed, "queued")
	if retry.Attempts != 1 || retry.Due < time.Now().Add(29*time.Minute).Unix() || !strings.Contains(retry.Reason, "中继认证失败") {
		t.Fatalf("unexpected auth retry: %+v", retry)
	}

	stale := queue("server interrupted")
	old, err := a.claimOutbox()
	if err != nil || old == nil || old.message.ID != stale {
		t.Fatalf("could not claim stale test job: %v", err)
	}
	if _, err := a.db.Exec(`UPDATE outbox SET lease=0 WHERE id=?`, stale); err != nil {
		t.Fatal(err)
	}
	replacement, err := a.claimOutbox()
	if err != nil || replacement == nil || replacement.message.ID != stale || replacement.claim == old.claim {
		t.Fatalf("pre-DATA job was not reclaimed: %v", err)
	}
	if _, err := a.db.Exec(`UPDATE outbox SET state='sending', lease=0 WHERE id=?`, stale); err != nil {
		t.Fatal(err)
	}
	if err := a.recoverOutbox(t.Context()); err != nil {
		t.Fatal(err)
	}
	checkState(stale, "uncertain")
}

func TestOutboxPhasesSyncAcrossInstances(t *testing.T) {
	dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set HARBOR_TEST_MYSQL_DSN to a disposable MySQL database")
	}
	writerDB, err := openDB("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer writerDB.Close()
	readerDB, err := openDB("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer readerDB.Close()
	writer := &app{db: writerDB, domain: "harbor.test", sharedMailboxEvents: true,
		relay: domainRelay{Address: "relay.harbor.test:587"}}
	reader := &app{db: readerDB, sharedMailboxEvents: true}
	owner := token()[:16] + "@harbor.test"
	m := letter{From: owner, To: "recipient@example.test", Subject: "shared outbox phases",
		Body: "test body", Date: time.Now().Unix()}
	raw, err := encodeLetter(m, writer.domain, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := writer.enqueue(m, raw)
	if err != nil {
		t.Fatal(err)
	}
	job, err := writer.claimOutbox()
	if err != nil || job == nil || job.message.ID != id {
		t.Fatalf("could not claim job %d: %v", id, err)
	}
	checkPhase := func(want string, revision int64) error {
		var state string
		if err := reader.db.QueryRowContext(t.Context(), `SELECT state FROM outbox WHERE id=? AND sender=?`, id, owner).
			Scan(&state); err != nil {
			return err
		}
		current, err := reader.mailboxRevision(t.Context(), owner)
		if err != nil || state != want || current != revision {
			return fmt.Errorf("state/revision: got %s/%d (%v), want %s/%d", state, current, err, want, revision)
		}
		return nil
	}
	if err := checkPhase("preparing", 1); err != nil {
		t.Error(err)
	}
	observed := make(chan error, 1)
	connect := serveTestRelay(t, writer, true, relayFixture{replyCode: 250, afterData: func() {
		observed <- checkPhase("sending", 2)
	}})
	if err := writer.processOutbox(job, connect); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-observed:
		if err != nil {
			t.Error(err)
		}
	default:
		t.Fatal("relay did not receive DATA")
	}
	if current, err := reader.mailboxRevision(t.Context(), owner); err != nil || current != 3 {
		t.Errorf("completion revision = %d (%v), want 3", current, err)
	}
}

func testRelay(t *testing.T, a *app, implicit bool, replyCode int) relayConnector {
	t.Helper()
	return serveTestRelay(t, a, implicit, relayFixture{replyCode: replyCode})
}

func serveTestRelay(t *testing.T, a *app, implicit bool, response relayFixture) relayConnector {
	t.Helper()
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := fixture.TLS.Certificates[0]
	roots := x509.NewCertPool()
	roots.AddCert(fixture.Certificate())
	fixture.Close()
	server := a.smtpServer(smtp.BackendFunc(func(conn *smtp.Conn) (smtp.Session, error) {
		session := response
		session.conn = conn
		return session, nil
	}), &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
	if implicit {
		listener = tls.NewListener(listener, server.TLSConfig)
	}
	go func() { _ = server.Serve(listener) }()
	return func(ctx context.Context) (*smtp.Client, func(), error) {
		return dialRelay(ctx, listener.Addr().String(), implicit,
			&tls.Config{ServerName: "127.0.0.1", RootCAs: roots, MinVersion: tls.VersionTLS12})
	}
}

type relayFixture struct {
	conn      *smtp.Conn
	replyCode int
	afterData func()
}

func (r relayFixture) Reset()                                   {}
func (r relayFixture) Logout() error                            { return nil }
func (r relayFixture) Mail(_ string, _ *smtp.MailOptions) error { return nil }
func (r relayFixture) Rcpt(_ string, _ *smtp.RcptOptions) error { return nil }
func (r relayFixture) Data(body io.Reader) error {
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if !strings.Contains(string(raw), "test body") {
		return &smtp.SMTPError{Code: 554, Message: "Empty test content"}
	}
	if r.afterData != nil {
		r.afterData()
	}
	switch r.replyCode {
	case 0:
		return r.conn.Close()
	case 250:
		return nil
	default:
		return &smtp.SMTPError{Code: r.replyCode, Message: "Simulated downstream response"}
	}
}
