package main

import (
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

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
	event := a.webLogin.recent[loginPairLimit]
	if event.Result != "blocked" || event.Protocol != "imap" || event.Source != "192.0.2.4" {
		t.Fatalf("wrong limit event: %+v", event)
	}
}
