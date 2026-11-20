package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
	"golang.org/x/crypto/bcrypt"
)

func TestAuthenticatedRelayRoundTrip(t *testing.T) {
	const defaultSender = "sender@harbor.test"
	const teamSender = "sender@team.harbor.test"
	const receiver = "receiver@remote.test"
	const mailboxPassword = "synthetic-mailbox-password"
	dir := t.TempDir()
	now := time.Now()
	certPEM, keyPEM := makeReloadTestPEM(t, 1, now.Add(-time.Hour), now.Add(time.Hour))
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(dir, "relay-ca.pem")
	if err := os.WriteFile(ca, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(mailboxPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	newHarbor := func(name string, owners ...string) (*app, *httptest.Server) {
		t.Helper()
		db, err := openDB("sqlite", filepath.Join(dir, name+".db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		a := &app{db: db, dbDriver: "sqlite", domain: name + ".test", accounts: make(map[string][]byte)}
		for _, owner := range owners {
			a.accounts[owner] = hash
		}
		web := httptest.NewServer(a.routes())
		t.Cleanup(web.Close)
		return a, web
	}
	local, localWeb := newHarbor("harbor", defaultSender, teamSender)
	remote, remoteWeb := newHarbor("remote", receiver)
	local.dkimCipher = testDKIMCipher(t)
	selector, public := installTestDKIMKey(t, local, "team.harbor.test")

	global := startRoundTripRelay(t, remote, pair, ca, "starttls", defaultSender, "default")
	team := startRoundTripRelay(t, remote, pair, ca, "implicit", teamSender, "team")
	reply := startRoundTripRelay(t, local, pair, ca, "starttls", receiver, "return")
	domains, err := json.Marshal(map[string]domainRelay{"team.harbor.test": team})
	if err != nil {
		t.Fatal(err)
	}
	domainFile := filepath.Join(dir, "domain-relays.json")
	if err := os.WriteFile(domainFile, domains, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARBOR_DOMAIN_RELAYS", "")
	t.Setenv("HARBOR_DOMAIN_RELAYS_FILE", domainFile)
	t.Setenv("HARBOR_RELAY_PASSWORD", "")
	setGlobal := func(relay domainRelay) {
		t.Helper()
		passwordFile := filepath.Join(dir, relay.User+".password")
		if err := os.WriteFile(passwordFile, []byte(relay.Password), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HARBOR_RELAY", relay.Address)
		t.Setenv("HARBOR_RELAY_TLS", relay.TLS)
		t.Setenv("HARBOR_RELAY_USER", relay.User)
		t.Setenv("HARBOR_RELAY_PASSWORD_FILE", passwordFile)
		t.Setenv("HARBOR_RELAY_CA_FILE", relay.CAFile)
	}
	setGlobal(global)
	if err := local.loadRelays(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARBOR_DOMAIN_RELAYS_FILE", "")
	setGlobal(reply)
	if err := remote.loadRelays(); err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	runtime := newServiceRuntime()
	t.Cleanup(runtime.shutdown)
	for _, a := range []*app{local, remote} {
		runtime.worker(func(ctx context.Context) { a.runOutbox(ctx, a.connectSenderRelay) })
	}
	post := func(client *http.Client, base, path string, data map[string]any) {
		t.Helper()
		body, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Post(base+path, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			details, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			t.Fatalf("%s: HTTP %d: %s", path, resp.StatusCode, details)
		}
	}
	loggedIn := func(web *httptest.Server, owner string) *http.Client {
		t.Helper()
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
		post(client, web.URL, "/api/login", map[string]any{"email": owner, "password": mailboxPassword})
		return client
	}
	send := func(client *http.Client, web *httptest.Server, to, subject string, replyTo int64) {
		t.Helper()
		post(client, web.URL, "/api/letters", map[string]any{
			"to": to, "subject": subject, "body": "Synthetic SMTP round trip", "replyToId": replyTo,
		})
	}
	received := func(a *app, owner, sender, folder, subject string) (int64, []byte) {
		t.Helper()
		deadline := time.Now().Add(12 * time.Second)
		for {
			var id int64
			var raw []byte
			err := a.db.QueryRowContext(ctx, `SELECT id,raw FROM letters WHERE owner=? AND sender=? AND folder=? AND subject=?`,
				owner, sender, folder, subject).Scan(&id, &raw)
			if err == nil {
				return id, raw
			}
			if time.Now().After(deadline) {
				t.Fatalf("waiting for %s message %q from %s to %s: %v", folder, subject, sender, owner, err)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}

	send(loggedIn(localWeb, defaultSender), localWeb, receiver, "Default route", 0)
	send(loggedIn(localWeb, teamSender), localWeb, receiver, "Team route", 0)
	received(local, defaultSender, defaultSender, "sent", "Default route")
	_, unsigned := received(remote, receiver, defaultSender, "inbox", "Default route")
	_, sent := received(local, teamSender, teamSender, "sent", "Team route")
	id, original := received(remote, receiver, teamSender, "inbox", "Team route")
	verifyTestDKIM(t, original, "team.harbor.test", selector, public)
	if bytes.HasPrefix(unsigned, []byte("DKIM-Signature:")) || bytes.HasPrefix(sent, []byte("DKIM-Signature:")) {
		t.Fatal("DKIM modified a domain without signing enabled or the locally stored original")
	}
	send(loggedIn(remoteWeb, receiver), remoteWeb, teamSender, "Re: Team route", id)
	received(remote, receiver, receiver, "sent", "Re: Team route")
	_, returned := received(local, teamSender, receiver, "inbox", "Re: Team route")
	parent, response := identifyMessage(original), identifyMessage(returned)
	if parent.id == "" || len(response.parents) != 1 || response.parents[0] != parent.id {
		t.Fatalf("reply lost its original Message-ID: parent=%q references=%v", parent.id, response.parents)
	}
}

func startRoundTripRelay(t *testing.T, target *app, pair tls.Certificate, ca, mode, from, name string) domainRelay {
	t.Helper()
	relay := domainRelay{TLS: mode, CAFile: ca, User: name, Password: name + "-password"}
	server := target.smtpServer(smtp.BackendFunc(func(conn *smtp.Conn) (smtp.Session, error) {
		session, err := target.NewSession(conn)
		return &roundTripRelaySession{Session: session, conn: conn, from: from, relay: relay}, err
	}), &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if mode == "implicit" {
		listener = tls.NewListener(listener, server.TLSConfig)
	}
	relay.Address = listener.Addr().String()
	t.Cleanup(func() { _ = listener.Close(); _ = server.Close() })
	go func() { _ = server.Serve(listener) }()
	return relay
}

type roundTripRelaySession struct {
	smtp.Session
	conn          *smtp.Conn
	from          string
	relay         domainRelay
	authenticated bool
}

func (*roundTripRelaySession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *roundTripRelaySession) Auth(mechanism string) (sasl.Server, error) {
	if mechanism != sasl.Plain {
		return nil, smtp.ErrAuthUnknownMechanism
	}
	if state, ok := s.conn.TLSConnectionState(); !ok || !state.HandshakeComplete {
		return nil, smtp.ErrAuthFailed
	}
	return sasl.NewPlainServer(func(_, user, password string) error {
		if user != s.relay.User || password != s.relay.Password {
			return smtp.ErrAuthFailed
		}
		s.authenticated = true
		return nil
	}), nil
}

func (s *roundTripRelaySession) Mail(from string, options *smtp.MailOptions) error {
	if !s.authenticated || from != s.from {
		return &smtp.SMTPError{Code: 550, Message: fmt.Sprintf("sender not authorized on %s relay", s.relay.User)}
	}
	return s.Session.Mail(from, options)
}
