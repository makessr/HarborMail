package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"net/textproto"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestInboundCapacityReservesBeforeReading(t *testing.T) {
	a := &app{}
	a.inbound.limits = inboundLimits{messages: 4, bytes: mailLimit, scans: 2}
	if !a.inbound.beginMessage(mailLimit) {
		t.Fatal("initial reservation rejected")
	}
	input := strings.NewReader("message data")
	s := &delivery{app: a, size: 1}
	if err := s.Data(input); !errors.Is(err, errInboundCapacity) || input.Len() != len("message data") {
		t.Fatalf("capacity error=%v, unread bytes=%d", err, input.Len())
	}
	a.inbound.endMessage(mailLimit)
	a.inbound.limits = inboundLimits{messages: 1, bytes: 2 * mailLimit, scans: 2}
	if !a.inbound.beginMessage(mailLimit) {
		t.Fatal("reservation was not released")
	}
	if err := s.Data(input); !errors.Is(err, errInboundCapacity) || input.Len() != len("message data") {
		t.Fatalf("message limit error=%v, unread bytes=%d", err, input.Len())
	}
	a.inbound.endMessage(mailLimit)
	if got := a.inbound.snapshot(); got != (inboundUsage{deferred: 2}) {
		t.Fatalf("inbound accounting: %+v", got)
	}
}

func TestInboundScanSaturationReturnsTemporarySMTPStatus(t *testing.T) {
	const account = "capacity@harbor.test"
	scanStarted := make(chan struct{})
	allowScan := make(chan struct{})
	releaseScan := sync.OnceFunc(func() { close(allowScan) })
	var scans atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if scans.Add(1) == 1 {
			close(scanStarted)
			select {
			case <-allowScan:
			case <-r.Context().Done():
				return
			}
		}
		fmt.Fprint(w, `{"action":"no action"}`)
	}))
	t.Cleanup(worker.Close)
	scanner, err := newRspamdScanner(worker.URL + "/checkv2")
	if err != nil {
		t.Fatal(err)
	}
	scanner.client.Timeout = 10 * time.Second
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := &app{db: db, domain: "harbor.test", accounts: map[string][]byte{account: nil}, spamScanner: scanner}
	a.inbound.limits = inboundLimits{messages: 2, bytes: 2 * mailLimit, scans: 1}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := a.smtpServer(a, nil)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = listener.Close(); _ = server.Close() })
	defer releaseScan()
	send := func(subject string) error {
		// Exercise the wire protocol, including DATA drain and the final reply.
		return smtp.SendMail(listener.Addr().String(), nil, "sender@example.test", []string{account},
			[]byte("Subject: "+subject+"\r\n\r\nsynthetic payload\r\n"))
	}
	first := make(chan error, 1)
	go func() { first <- send("first") }()
	select {
	case <-scanStarted:
	case err := <-first:
		t.Fatalf("delivery ended before scanning: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("scanner was not called")
	}
	var smtpError *textproto.Error
	if err := send("retry"); !errors.As(err, &smtpError) || smtpError.Code != 451 {
		t.Fatalf("overload returned %v", err)
	}
	if got := a.inbound.snapshot(); got != (inboundUsage{messages: 1, bytes: mailLimit, scans: 1, deferred: 1}) {
		t.Fatalf("wrong active limits: %+v", got)
	}
	releaseScan()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := send("retry"); err != nil {
		t.Fatalf("retry after capacity recovered: %v", err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM letters WHERE owner=?", account).Scan(&count); err != nil || count != 2 {
		t.Fatalf("stored messages: %d, %v", count, err)
	}
	if got := a.inbound.snapshot(); got != (inboundUsage{deferred: 1}) || scans.Load() != 2 {
		t.Fatalf("unreleased capacity or scan bypass: %+v, scans=%d", got, scans.Load())
	}
}
