package server

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

func TestShutdownDrainsOutbox(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, domain: "harbor.test", relay: domainRelay{Address: "relay.harbor.test:587"}}
	queue := func(subject string) int64 {
		t.Helper()
		message := letter{From: "sender@harbor.test", To: "recipient@example.test", Subject: subject,
			Body: "test body", Date: time.Now().Unix()}
		raw, err := encodeLetter(message, a.domain, nil)
		if err != nil {
			t.Fatal(err)
		}
		id, err := a.enqueue(message, raw)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first, second := queue("Finish before shutdown"), queue("Leave queued during shutdown")
	received, release := make(chan struct{}, 1), make(chan struct{})
	connect := serveTestRelay(t, a, true, relayFixture{replyCode: 250, afterData: func() {
		received <- struct{}{}
		select {
		case <-release:
		case <-t.Context().Done():
		}
	}})
	runtime := newServiceRuntime()
	defer runtime.cancel()
	runtime.worker(func(ctx context.Context) {
		a.runOutbox(ctx, func(ctx context.Context, _ string) (*smtp.Client, func(), error) { return connect(ctx) })
	})
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("outbox did not reach SMTP DATA")
	}
	stopped := make(chan struct{})
	go func() {
		runtime.shutdown()
		close(stopped)
	}()
	select {
	case <-runtime.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not start")
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("outbox did not finish before shutdown")
	}
	var finished bool
	if err := db.QueryRow(`SELECT NOT EXISTS(SELECT 1 FROM outbox WHERE id=?)`, first).Scan(&finished); err != nil || !finished {
		t.Fatalf("active outbox task did not finish: %v (%v)", finished, err)
	}
	var state string
	var attempts int
	if err := db.QueryRow(`SELECT state,attempts FROM outbox WHERE id=?`, second).Scan(&state, &attempts); err != nil ||
		state != "queued" || attempts != 0 {
		t.Fatalf("next outbox task: state=%s attempts=%d (%v)", state, attempts, err)
	}
}

func TestShutdownDrainsSMTP(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testShutdownDrainsSMTP(t, "sqlite", filepath.Join(t.TempDir(), "mail.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testShutdownDrainsSMTP(t, "mysql", dsn) })
	}
}

func testShutdownDrainsSMTP(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := token()[:12] + "@harbor.test"
	runtime := newServiceRuntime()
	defer runtime.cancel()
	a := &app{db: db, domain: "harbor.test", stopping: runtime.ctx.Done(), accounts: map[string][]byte{owner: nil}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	smtpServer := a.smtpServer(a, nil)
	runtime.serveSMTP("SMTP", smtpServer, listener)
	active, err := smtp.Dial(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	idle, err := smtp.Dial(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	if err := idle.Hello("client.example.test"); err != nil {
		t.Fatal(err)
	}
	if err := active.Mail("sender@example.test", nil); err != nil {
		t.Fatal(err)
	}
	if err := active.Rcpt(owner, nil); err != nil {
		t.Fatal(err)
	}
	data, err := active.Data()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(data, "Subject: Drain active SMTP\r\n\r\nBefore shutdown\r\n"); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		runtime.shutdown()
		close(stopped)
	}()
	select {
	case <-runtime.ctx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not start")
	}
	var smtpError *smtp.SMTPError
	if err := idle.Mail("sender@example.test", nil); !errors.As(err, &smtpError) || smtpError.Code != 421 {
		t.Fatalf("new transaction: want 421, got %v", err)
	}
	_ = idle.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", listener.Addr().String(), 100*time.Millisecond)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("SMTP listener stayed open during shutdown")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-stopped:
		t.Fatal("shutdown completed before the active SMTP transaction")
	default:
	}
	if err := data.Close(); err != nil {
		t.Fatalf("in-flight SMTP transaction: %v", err)
	}
	if err := active.Quit(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not complete after SMTP disconnected")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM letters WHERE owner=? AND subject='Drain active SMTP'`, owner).
		Scan(&count); err != nil || count != 1 {
		t.Fatalf("stored mail: count=%d (%v)", count, err)
	}
}
