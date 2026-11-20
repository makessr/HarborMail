package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/crypto/bcrypt"
)

func TestAdminDKIMLifecycle(t *testing.T) {
	const domain = "mail.test"
	const endpoint = "/api/admin/domains/" + domain + "/dkim"
	const password = "disposable-admin-password"
	path := filepath.Join(t.TempDir(), "admin.db")
	db, err := openDB("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t.Setenv("HARBOR_DB_DRIVER", "sqlite")
	t.Setenv("HARBOR_DB", path)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer((&app{db: db, dbDriver: "sqlite", dkimCipher: testDKIMCipher(t), domain: domain,
		admins:   map[string]bool{"admin@" + domain: true},
		accounts: map[string][]byte{"admin@" + domain: hash, "user@" + domain: hash}}).routes())
	defer web.Close()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var published atomic.Pointer[dkimPublicKey]
	dnsServer := &dns.Server{PacketConn: conn, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		response := new(dns.Msg).SetReply(request)
		response.Authoritative = true
		question := request.Question[0]
		if record := published.Load(); record != nil && question.Name == record.Name+"." && question.Qtype == dns.TypeTXT {
			response.Answer = []dns.RR{&dns.TXT{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET},
				Txt: []string{record.TXT[:200], record.TXT[200:]}}}
		}
		_ = w.WriteMsg(response)
	})}
	go func() { _ = dnsServer.ActivateAndServe() }()
	defer func() { _ = dnsServer.Shutdown(); _ = conn.Close() }()
	resolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, conn.LocalAddr().String())
	}}
	defer func() { net.DefaultResolver = resolver }()

	post := func(client *http.Client, path string, input map[string]string, status int) domainDKIMStatus {
		t.Helper()
		data, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Post(web.URL+path, "application/json", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != status {
			t.Fatalf("%s: HTTP %d, expected %d", path, response.StatusCode, status)
		}
		var result domainDKIMStatus
		if path == endpoint && status == http.StatusOK {
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	login := func(email string) *http.Client {
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
		post(client, "/api/login", map[string]string{"email": email, "password": password}, http.StatusOK)
		return client
	}
	change := func(client *http.Client, action, selector, proof string, status int) domainDKIMStatus {
		return post(client, endpoint, map[string]string{"action": action, "selector": selector, "password": proof}, status)
	}
	check := func(keyState, dnsState, slot string, args ...string) {
		t.Helper()
		var out bytes.Buffer
		err := runDKIMCheck(&out, net.DefaultResolver, append([]string{domain}, args...))
		if (err == nil) != (keyState == "match" && dnsState == "match") {
			t.Fatalf("unexpected preflight status: %v", err)
		}
		var result dkimCheckReport
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.KeyState != keyState || result.DNSState != dnsState || result.Slot != slot {
			t.Fatalf("unexpected preflight report: %+v", result)
		}
	}
	admin := login("admin@" + domain)
	change(login("user@"+domain), "generate", "", password, http.StatusForbidden)
	change(admin, "generate", "", "wrong", http.StatusForbidden)
	first := change(admin, "generate", "", password, http.StatusOK)
	if !first.Available || first.Active != nil || first.Pending == nil || !strings.HasPrefix(first.Pending.TXT, "v=DKIM1; k=rsa; p=") {
		t.Fatal("key generation must only return a pending public record")
	}
	check("not_configured", "skipped", "active")
	check("match", "missing", "pending", "--pending")
	change(admin, "enable", first.Pending.Selector, password, http.StatusConflict)
	wrongRecord := *first.Pending
	wrongRecord.TXT += "x"
	published.Store(&wrongRecord)
	check("match", "mismatch", "pending", "--pending")
	published.Store(first.Pending)
	check("match", "match", "pending", "--pending")
	enabled := change(admin, "enable", first.Pending.Selector, password, http.StatusOK)
	if enabled.Active == nil || enabled.Active.Selector != first.Pending.Selector || enabled.Pending != nil {
		t.Fatal("DNS confirmation must activate the pending key")
	}
	check("match", "match", "active")
	t.Setenv("HARBOR_DKIM_KEY", "")
	check("secret_missing", "match", "active")
	t.Setenv("HARBOR_DKIM_KEY", "not-hex")
	check("secret_unusable", "match", "active")
	t.Setenv("HARBOR_DKIM_KEY", strings.Repeat("70", 32))
	check("decrypt_failed", "match", "active")
	t.Setenv("HARBOR_DKIM_KEY", strings.Repeat("69", 32))
	rotate := change(admin, "generate", "", password, http.StatusOK)
	if rotate.Active == nil || rotate.Active.Selector != enabled.Active.Selector || rotate.Pending == nil || rotate.Pending.Selector == rotate.Active.Selector {
		t.Fatal("generating a rotation key must leave the current key active")
	}
	published.Store(rotate.Pending)
	rotated := change(admin, "enable", rotate.Pending.Selector, password, http.StatusOK)
	if rotated.Active == nil || rotated.Active.Selector != rotate.Pending.Selector {
		t.Fatal("new key was not activated")
	}
	change(admin, "disable", enabled.Active.Selector, password, http.StatusConflict)
	if disabled := change(admin, "disable", rotated.Active.Selector, password, http.StatusOK); disabled.Active != nil {
		t.Fatal("active signing key was not disabled")
	}
}
