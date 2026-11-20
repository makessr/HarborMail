package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"github.com/miekg/dns"
)

const cloudflareTestHost = "mail.harbor.test"
const cloudflareTestToken = "synthetic-dns-token"

func cloudflareTestEnv(t *testing.T, dir string) {
	t.Helper()
	for key, value := range map[string]string{
		"HARBOR_ACME_CHALLENGE": "cloudflare", "HARBOR_ACME_HOSTS": cloudflareTestHost,
		"HARBOR_ACME_ACCEPT_TOS": "true", "HARBOR_ACME_CACHE": dir, "HARBOR_ACME_DIRECTORY": "",
		"HARBOR_CERT": "", "HARBOR_KEY": "", "HARBOR_ACME_CLOUDFLARE_TOKEN": "", "HARBOR_ACME_CLOUDFLARE_TOKEN_FILE": "",
	} {
		t.Setenv(key, value)
	}
}

func TestCloudflareACMEConfiguration(t *testing.T) {
	dir := t.TempDir()
	cloudflareTestEnv(t, dir)
	if _, err := configureTLS(); err == nil || !strings.Contains(err.Error(), "HARBOR_ACME_CLOUDFLARE_TOKEN") {
		t.Fatalf("missing token: %v", err)
	}
	path := filepath.Join(dir, "dns-token")
	if err := os.WriteFile(path, []byte(cloudflareTestToken+"\r\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARBOR_ACME_CLOUDFLARE_TOKEN_FILE", path)
	t.Setenv("HARBOR_ACME_CLOUDFLARE_TOKEN", "duplicate")
	if _, err := configureTLS(); err == nil || !strings.Contains(err.Error(), "set only one") {
		t.Fatalf("conflicting token settings: %v", err)
	}
	t.Setenv("HARBOR_ACME_CLOUDFLARE_TOKEN", "")
	inbound, err := configureTLS()
	if err != nil {
		t.Fatal(err)
	}
	if inbound.dns == nil || inbound.acme != nil || inbound.pem != nil {
		t.Fatal("expected DNS-01 without an HTTP challenge manager")
	}
	t.Cleanup(inbound.dns.cache.Stop)
	issuer := inbound.dns.manager.Issuers[0].(*certmagic.ACMEIssuer)
	solver := issuer.DNS01Solver.(*certmagic.DNS01Solver)
	provider := solver.DNSProvider.(*cloudflare.Provider)
	if !issuer.DisableHTTPChallenge || !issuer.DisableTLSALPNChallenge ||
		issuer.CA != certmagic.LetsEncryptProductionCA || provider.APIToken != cloudflareTestToken {
		t.Fatal("unexpected DNS-01 issuer configuration")
	}
	if _, err := inbound.config.GetCertificate(&tls.ClientHelloInfo{ServerName: "unconfigured.harbor.test"}); err == nil {
		t.Fatal("unconfigured SNI was accepted")
	}
}

func TestCloudflareDNS01(t *testing.T) {
	pebble := os.Getenv("HARBOR_TEST_PEBBLE")
	if pebble == "" {
		t.Skip("set HARBOR_TEST_PEBBLE to the Pebble executable")
	}
	dir := t.TempDir()
	cloudflareTestEnv(t, filepath.Join(dir, "cache"))
	t.Setenv("HARBOR_ACME_CLOUDFLARE_TOKEN", cloudflareTestToken)
	t.Setenv("HARBOR_ACME_EMAIL", "probe@harbor.test")
	addresses := acmeTestAddresses(t, []string{"pebble", "pebble-admin", "closed-validation-port"})
	directory := "https://" + addresses["pebble"] + "/dir"
	t.Setenv("HARBOR_ACME_DIRECTORY", directory)
	fixture := &testCloudflareDNS{}
	resolver := fixture.listenDNS(t)
	closed := addresses["closed-validation-port"]
	api, _ := startTestPebble(t, pebble, dir, resolver, addresses["pebble"], addresses["pebble-admin"], closed, closed)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(acmeTestGet(t, api, "https://"+addresses["pebble-admin"]+"/roots/0")) {
		t.Fatal("Pebble did not return a root certificate")
	}

	inbound, err := configureTLS()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(inbound.dns.cache.Stop)
	manager := inbound.dns.manager
	issuer := manager.Issuers[0].(*certmagic.ACMEIssuer)
	solver := issuer.DNS01Solver.(*certmagic.DNS01Solver)
	solver.DNSProvider.(*cloudflare.Provider).HTTPClient = fixture
	solver.Resolvers = []string{resolver}
	// Pebble still queries the published TXT. This fixture has no public
	// authoritative nameserver for CertMagic's separate propagation check.
	solver.PropagationTimeout = -1
	manager.Issuers = []certmagic.Issuer{certmagic.NewACMEIssuer(manager, certmagic.ACMEIssuer{
		CA: issuer.CA, TestCA: issuer.TestCA, Email: issuer.Email, Agreed: issuer.Agreed, DNS01Solver: solver,
		DisableHTTPChallenge: issuer.DisableHTTPChallenge, DisableTLSALPNChallenge: issuer.DisableTLSALPNChallenge,
		TrustedRoots: api.Transport.(*http.Transport).TLSClientConfig.RootCAs,
	})}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := manager.ManageSync(ctx, inbound.dns.hosts); err != nil {
		t.Fatal(err)
	}
	// Leave the validation listeners closed. Only the application TLS listener is started.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	runtime := newServiceRuntime()
	t.Cleanup(runtime.shutdown)
	runtime.serveHTTP("test DNS HTTPS", &http.Server{TLSConfig: inbound.config.Clone()}, listener)
	conn, err := acmeTestDial(listener.Addr().String(), &tls.Config{
		ServerName: cloudflareTestHost, RootCAs: roots, MinVersion: tls.VersionTLS12,
	}, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if err := manager.RenewCertSync(ctx, cloudflareTestHost, true); err != nil {
		t.Fatal(err)
	}
	renewed, err := manager.ClientCredentials(ctx, inbound.dns.hosts)
	if err != nil {
		t.Fatal(err)
	}
	if renewed[0].Leaf.SerialNumber.Cmp(conn.ConnectionState().PeerCertificates[0].SerialNumber) == 0 {
		t.Fatal("DNS-01 renewal did not issue a new serial number")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.writes != 2 || fixture.deletes != 2 || fixture.record != nil {
		t.Fatalf("expected two complete TXT challenge lifecycles, got %d writes and %d deletions", fixture.writes, fixture.deletes)
	}
}

type testCloudflareDNS struct {
	mu      sync.Mutex
	record  map[string]any
	writes  int
	deletes int
}

func (f *testCloudflareDNS) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "api.cloudflare.com" || req.Header.Get("Authorization") != "Bearer "+cloudflareTestToken {
		return nil, fmt.Errorf("unexpected Cloudflare API host or token")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var result any
	switch req.Method + " " + req.URL.Path {
	case "GET /client/v4/zones":
		if strings.TrimSuffix(req.URL.Query().Get("name"), ".") != "harbor.test" {
			return nil, fmt.Errorf("unexpected Cloudflare zone: %q", req.URL.Query().Get("name"))
		}
		result = []map[string]string{{"id": "zone", "name": "harbor.test"}}
	case "POST /client/v4/zones/zone/dns_records":
		var record map[string]any
		if err := json.NewDecoder(req.Body).Decode(&record); err != nil {
			return nil, err
		}
		if f.record != nil || record["type"] != "TXT" || record["name"] != "_acme-challenge.mail" {
			return nil, fmt.Errorf("expected a new ACME TXT record, got %v", record)
		}
		record["id"] = "challenge"
		record["name"] = "_acme-challenge." + cloudflareTestHost
		f.record = record
		f.writes++
		result = record
	case "GET /client/v4/zones/zone/dns_records":
		result = []map[string]any{f.record}
	case "DELETE /client/v4/zones/zone/dns_records/challenge":
		result = f.record
		f.record = nil
		f.deletes++
	default:
		return nil, fmt.Errorf("unexpected Cloudflare API request: %s %s", req.Method, req.URL.Path)
	}
	resp := httptest.NewRecorder()
	err := json.NewEncoder(resp).Encode(map[string]any{"success": true, "result": result})
	return resp.Result(), err
}

func (f *testCloudflareDNS) listenDNS(t *testing.T) string {
	t.Helper()
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tcp.Close() })
	udp, err := net.ListenPacket("udp4", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = udp.Close() })
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		response := new(dns.Msg).SetReply(request)
		for _, question := range request.Question {
			header := dns.RR_Header{Name: question.Name, Rrtype: question.Qtype, Class: dns.ClassINET, Ttl: 1}
			switch {
			case question.Qtype == dns.TypeSOA && question.Name == "harbor.test.":
				response.Answer = append(response.Answer, &dns.SOA{Hdr: header, Ns: "ns.harbor.test.", Mbox: "admin.harbor.test."})
			case question.Qtype == dns.TypeTXT && question.Name == "_acme-challenge."+cloudflareTestHost+".":
				f.mu.Lock()
				if content, ok := f.record["content"].(string); ok {
					response.Answer = append(response.Answer, &dns.TXT{Hdr: header, Txt: []string{strings.Trim(content, "\"")}})
				}
				f.mu.Unlock()
			}
		}
		_ = w.WriteMsg(response)
	})
	for _, server := range []*dns.Server{{Listener: tcp, Handler: handler}, {PacketConn: udp, Handler: handler}} {
		ready := make(chan struct{})
		server.NotifyStartedFunc = func() { close(ready) }
		go func() { _ = server.ActivateAndServe() }()
		select {
		case <-ready:
			t.Cleanup(func() { _ = server.Shutdown() })
		case <-time.After(time.Second):
			t.Fatal("local DNS fixture did not start")
		}
	}
	return tcp.Addr().String()
}
