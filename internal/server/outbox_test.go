package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
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
	if err := a.recoverOutbox(); err != nil {
		t.Fatal(err)
	}
	checkState(stale, "uncertain")
}

func testRelay(t *testing.T, a *app, implicit bool, replyCode int) relayConnector {
	t.Helper()
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := fixture.TLS.Certificates[0]
	roots := x509.NewCertPool()
	roots.AddCert(fixture.Certificate())
	fixture.Close()
	server := a.smtpServer(smtp.BackendFunc(func(conn *smtp.Conn) (smtp.Session, error) {
		return relayFixture{conn: conn, replyCode: replyCode}, nil
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
	switch r.replyCode {
	case 0:
		return r.conn.Close()
	case 250:
		return nil
	default:
		return &smtp.SMTPError{Code: r.replyCode, Message: "Simulated downstream response"}
	}
}
