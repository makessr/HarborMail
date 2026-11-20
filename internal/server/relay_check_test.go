package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

func TestRelayCheck(t *testing.T) {
	for _, mode := range []string{"starttls", "implicit"} {
		t.Run(mode, func(t *testing.T) { checkRelayMode(t, mode) })
	}
}

func checkRelayMode(t *testing.T, mode string) {
	t.Helper()
	for _, key := range []string{"HARBOR_RELAY", "HARBOR_RELAY_TLS", "HARBOR_RELAY_USER", "HARBOR_RELAY_PASSWORD",
		"HARBOR_RELAY_PASSWORD_FILE", "HARBOR_RELAY_CA_FILE", "HARBOR_DOMAIN_RELAYS", "HARBOR_DOMAIN_RELAYS_FILE"} {
		t.Setenv(key, "")
	}
	var out bytes.Buffer
	if err := runRelayCheck(&out, "user@example.com", "--invalid"); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("invalid option: %v", err)
	}
	if err := runRelayCheck(&out, "user@example.com"); err == nil || !strings.Contains(err.Error(), "no SMTP relay") {
		t.Fatalf("missing relay: %v", err)
	}
	if err := runRelayCheck(&out, "Sender <user@example.com>"); err == nil || !strings.Contains(err.Error(), "plain sender email") {
		t.Fatalf("invalid sender: %v", err)
	}

	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	cert := fixture.TLS.Certificates[0]
	fixture.Close()
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	passwordPath := filepath.Join(dir, "password")
	domainPath := filepath.Join(dir, "domain-relays.json")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwordPath, []byte("fixture-password\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var envelopeCommands, resets, deliveries atomic.Int32
	a := &app{}
	server := a.smtpServer(smtp.BackendFunc(func(*smtp.Conn) (smtp.Session, error) {
		return &relayCheckSession{envelopes: &envelopeCommands, resets: &resets, deliveries: &deliveries}, nil
	}), &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if mode == "implicit" {
		listener = tls.NewListener(listener, server.TLSConfig)
		t.Setenv("HARBOR_RELAY_TLS", mode)
	}
	t.Cleanup(func() { _ = listener.Close(); _ = server.Close() })
	go func() { _ = server.Serve(listener) }()
	t.Setenv("HARBOR_RELAY", listener.Addr().String())
	t.Setenv("HARBOR_RELAY_USER", "fixture-user")
	t.Setenv("HARBOR_RELAY_PASSWORD_FILE", passwordPath)

	out.Reset()
	err = runRelayCheck(&out, "user@example.com")
	if err == nil || !strings.Contains(err.Error(), "certificate verification failed") {
		t.Fatalf("untrusted relay: %v", err)
	}
	t.Setenv("HARBOR_RELAY_CA_FILE", caPath)
	out.Reset()
	if err := runRelayCheck(&out, "user@EXAMPLE.COM"); err != nil ||
		!strings.Contains(out.String(), "(global, "+mode+")") || !strings.Contains(out.String(), "authentication verified") {
		t.Fatalf("global relay: %s %v", out.String(), err)
	}
	domainRelayJSON, err := json.Marshal(map[string]domainRelay{
		"team.example.com": {Address: listener.Addr().String(), TLS: mode, CAFile: caPath},
		"bad.example.com":  {Address: listener.Addr().String(), TLS: mode, CAFile: caPath, User: "fixture-user", Password: "bad-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(domainPath, domainRelayJSON, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARBOR_DOMAIN_RELAYS_FILE", domainPath)
	out.Reset()
	if err := runRelayCheck(&out, "user@team.example.com"); err != nil ||
		!strings.Contains(out.String(), "(per-domain, "+mode+")") || !strings.Contains(out.String(), "authentication is not configured") {
		t.Fatalf("per-domain relay: %s %v", out.String(), err)
	}
	out.Reset()
	err = runRelayCheck(&out, "user@bad.example.com", "--mail-from")
	if err == nil || err.Error() != "SMTP relay authentication failed (SMTP 535)" ||
		strings.Contains(out.String(), "bad-secret") || strings.Contains(out.String(), "fixture-user") {
		t.Fatalf("credential redaction: %s %v", out.String(), err)
	}
	if count := envelopeCommands.Load(); count != 0 {
		t.Fatalf("unexpected MAIL FROM commands without a successful opt-in: %d", count)
	}

	out.Reset()
	if err := runRelayCheck(&out, "user@TEAM.EXAMPLE.COM", "--mail-from"); err != nil ||
		!strings.Contains(out.String(), "(per-domain, "+mode+")") || !strings.Contains(out.String(), "accepted MAIL FROM and reset") {
		t.Fatalf("sender probe: %s %v", out.String(), err)
	}
	out.Reset()
	err = runRelayCheck(&out, "blocked@example.com", "--mail-from")
	if err == nil || err.Error() != "MAIL FROM: SMTP relay check failed (SMTP 550)" ||
		strings.Contains(out.String(), "accepted MAIL FROM") || !strings.Contains(out.String(), "authentication verified") {
		t.Fatalf("sender rejection: %s %v", out.String(), err)
	}
	if envelopeCommands.Load() != 2 || resets.Load() != 1 || deliveries.Load() != 0 {
		t.Fatal("sender probe must reset the accepted envelope without using RCPT or DATA")
	}
}

type relayCheckSession struct {
	envelopes, resets, deliveries *atomic.Int32
	accepted                      bool
}

func (s *relayCheckSession) Mail(from string, _ *smtp.MailOptions) error {
	s.envelopes.Add(1)
	if from != "user@team.example.com" {
		return &smtp.SMTPError{Code: 550, Message: "Rejected sender; private relay policy"}
	}
	s.accepted = true
	return nil
}

func (s *relayCheckSession) Reset() {
	if s.accepted {
		s.resets.Add(1)
		s.accepted = false
	}
}

func (s *relayCheckSession) Logout() error { return nil }

func (s *relayCheckSession) Rcpt(_ string, _ *smtp.RcptOptions) error {
	s.deliveries.Add(1)
	return errors.New("unexpected RCPT")
}

func (s *relayCheckSession) Data(io.Reader) error {
	s.deliveries.Add(1)
	return errors.New("unexpected DATA")
}

func (s *relayCheckSession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *relayCheckSession) Auth(mechanism string) (sasl.Server, error) {
	if mechanism != sasl.Plain {
		return nil, smtp.ErrAuthUnknownMechanism
	}
	return sasl.NewPlainServer(func(_, username, password string) error {
		if username == "fixture-user" && password == "fixture-password" {
			return nil
		}
		return &smtp.SMTPError{Code: 535, Message: "Rejected " + username + " " + password}
	}), nil
}
