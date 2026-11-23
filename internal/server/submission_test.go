package server

import (
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersion/go-smtp"
	"golang.org/x/crypto/bcrypt"
)

type submissionReader func([]byte) (int, error)

func (read submissionReader) Read(p []byte) (int, error) { return read(p) }

func TestSubmissionCapacityReleasesReservations(t *testing.T) {
	a := &app{submissions: inboundCapacity{limits: inboundLimits{messages: 1, bytes: mailLimit}}}
	newSession := func() *submission {
		return &submission{app: a, recipients: []webTarget{{Address: "recipient@harbor.test"}}}
	}
	reading, disconnect := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- newSession().Data(submissionReader(func([]byte) (int, error) {
			close(reading)
			<-disconnect
			return 0, io.ErrUnexpectedEOF
		}))
	}()
	<-reading
	err := newSession().Data(submissionReader(func([]byte) (int, error) {
		t.Error("over-capacity submission read the message body")
		return 0, io.EOF
	}))
	close(disconnect)
	if err := <-done; !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("disconnected submission: %v", err)
	}
	var smtpError *smtp.SMTPError
	if !errors.As(err, &smtpError) || smtpError.Code != 451 {
		t.Fatalf("over-capacity submission: %v", err)
	}
	if got := a.submissions.snapshot(); got.messages != 0 || got.bytes != 0 || got.deferred != 1 {
		t.Fatalf("submission budget after disconnect: %+v", got)
	}
	if !a.inbound.beginMessage(mailLimit) {
		t.Fatal("inbound and submission must have separate budgets")
	}
	defer a.inbound.endMessage(mailLimit)
	if err := newSession().Data(strings.NewReader(strings.Repeat("x", mailLimit+1))); !errors.Is(err, smtp.ErrDataTooLarge) {
		t.Fatalf("oversized submission: %v", err)
	}
	if got := a.submissions.snapshot(); got.messages != 0 || got.bytes != 0 {
		t.Fatalf("submission budget after oversized message: %+v", got)
	}
}

func TestSubmissionStoresOneSentCopyForMultipleLocalRecipients(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const sender, visible, blind = "sender@harbor.test", "to@harbor.test", "hidden@harbor.test"
	a := &app{db: db, accounts: map[string][]byte{sender: {}, visible: {}, blind: {}}}
	session := &submission{app: a, owner: sender}
	if err := session.Mail(sender, nil); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{visible, blind, blind} {
		if err := session.Rcpt(target, nil); err != nil {
			t.Fatal(err)
		}
	}
	raw := "From: " + sender + "\r\nTo: " + visible + "\r\nBcc: " + blind + "\r\nSubject: hello\r\n\r\nHi.\r\n"
	if err := session.Data(strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if got := a.submissions.snapshot(); got.messages != 0 || got.bytes != 0 {
		t.Fatalf("submission budget after successful delivery: %+v", got)
	}
	for _, address := range []string{sender, visible, blind} {
		var count int
		var exposed bool
		err := db.QueryRow(`SELECT COUNT(*),COALESCE(MAX(instr(CAST(raw AS TEXT),'Bcc:')>0),FALSE) FROM letters WHERE owner=?`, address).
			Scan(&count, &exposed)
		if err != nil || count != 1 || exposed {
			t.Fatalf("%s: count=%d exposed=%v err=%v", address, count, exposed, err)
		}
	}
	var bcc string
	if err := db.QueryRow(`SELECT r.bcc_list FROM web_letter_recipients r JOIN letters l ON l.id=r.letter_id WHERE l.owner=?`, sender).
		Scan(&bcc); err != nil || bcc != blind {
		t.Fatalf("sender's Bcc = %q, %v", bcc, err)
	}
}

func TestProtocolLoginLimitsAttemptsAcrossClients(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const owner, password = "owner@harbor.test", "protocol-test-password"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, accounts: map[string][]byte{owner: hash}}
	peer := &net.TCPAddr{IP: net.IPv4(192, 0, 2, 4), Port: 10000}
	protocols := []mailProtocol{clientSMTP, clientIMAP, clientPOP3}
	for i := range loginPairLimit {
		peer.Port++
		if _, err := a.authenticateProtocol(peer, owner, "wrong", protocols[i%len(protocols)]); !errors.Is(err, errLoginInvalid) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if _, err := a.authenticateProtocol(peer, owner, password, clientIMAP); !errors.Is(err, errLoginBusy) {
		t.Fatalf("cross-protocol limit: %v", err)
	}
	if _, err := a.authenticate(owner, password); err != nil {
		t.Fatalf("Web and protocol authentication should be independent: %v", err)
	}
	var event loginEvent
	if err := db.QueryRow(`SELECT result,protocol,source,account FROM login_audit_events ORDER BY id DESC LIMIT 1`).
		Scan(&event.Result, &event.Protocol, &event.Source, &event.Account); err != nil {
		t.Fatal(err)
	}
	if event.Result != "blocked" || event.Protocol != "imap" || event.Source != "192.0.2.4" || event.Account != owner {
		t.Fatalf("wrong limit event: %+v", event)
	}
}
