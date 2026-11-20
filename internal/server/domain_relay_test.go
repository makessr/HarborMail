package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

func TestSenderRelayPrivateCA(t *testing.T) {
	certificateServer := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := certificateServer.TLS.Certificates[0]
	bundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
	certificateServer.Close()
	bundlePath := filepath.Join(t.TempDir(), "relay-ca.pem")
	if err := os.WriteFile(bundlePath, bundle, 0600); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []string{"starttls", "implicit"} {
		t.Run(mode, func(t *testing.T) {
			a := &app{}
			server := a.smtpServer(smtp.BackendFunc(func(conn *smtp.Conn) (smtp.Session, error) {
				return relayFixture{conn: conn, replyCode: 250}, nil
			}), &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "implicit" {
				listener = tls.NewListener(listener, server.TLSConfig)
			}
			t.Cleanup(func() { _ = listener.Close(); _ = server.Close() })
			go func() { _ = server.Serve(listener) }()

			a.relay = domainRelay{Address: listener.Addr().String(), TLS: mode}
			_, port, err := net.SplitHostPort(a.relay.Address)
			if err != nil {
				t.Fatal(err)
			}
			domainConfig, err := json.Marshal(map[string]domainRelay{
				"team.example.com":  {Address: a.relay.Address, TLS: mode, CAFile: bundlePath},
				"other.example.com": {Address: a.relay.Address, TLS: mode},
				"wrong.example.com": {Address: net.JoinHostPort("localhost", port), TLS: mode, CAFile: bundlePath},
			})
			if err != nil {
				t.Fatal(err)
			}
			a.domainRelays, err = parseDomainRelays(string(domainConfig))
			if err != nil {
				t.Fatal(err)
			}

			connect := func(owner string) error {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				client, closeClient, err := a.connectSenderRelay(ctx, owner)
				if err != nil {
					return err
				}
				defer closeClient()
				return client.Mail(owner, nil)
			}
			var unknownAuthority x509.UnknownAuthorityError
			if err := connect("user@example.com"); !errors.As(err, &unknownAuthority) {
				t.Fatalf("global relay without the private CA: got %v", err)
			}
			if err := connect("user@team.example.com"); err != nil {
				t.Fatalf("per-domain relay with the private CA: %v", err)
			}
			a.relay.CAFile = bundlePath
			if err := a.relay.configure(); err != nil {
				t.Fatal(err)
			}
			if err := connect("user@example.com"); err != nil {
				t.Fatalf("global relay with the private CA: %v", err)
			}
			if err := connect("user@other.example.com"); !errors.As(err, &unknownAuthority) {
				t.Fatalf("per-domain relay inherited global trust: got %v", err)
			}
			var wrongHostname x509.HostnameError
			if err := connect("user@wrong.example.com"); !errors.As(err, &wrongHostname) {
				t.Fatalf("relay with a mismatched hostname: got %v", err)
			}
		})
	}
}

func TestRelayRejectsInvalidCAFiles(t *testing.T) {
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid.pem")
	if err := os.WriteFile(invalid, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	relay := domainRelay{Address: "smtp.example.com:587", TLS: "starttls", CAFile: invalid}
	if err := relay.configure(); err == nil {
		t.Fatal("global relay accepted an invalid CA bundle")
	}
	relay.CAFile = filepath.Join(dir, "missing.pem")
	raw, err := json.Marshal(map[string]domainRelay{"example.com": relay})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseDomainRelays(string(raw)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("per-domain relay with a missing CA bundle: got %v", err)
	}
}
