package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestAdminDomainDNSPlan(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const domain, other, actor = "example.test", "other.test", "operator@example.test"
	a := &app{db: db, domainAdmins: map[string]map[string]bool{actor: {domain: true}}}
	if _, err := db.Exec("INSERT INTO managed_domains(name) VALUES (?), (?)", domain, other); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARBOR_CLIENT_HOST", "")
	request := func(name, query string, want int) []mailDNSRecord {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/api/admin/domains/"+name+"/dns-plan"+query, nil)
		r.SetPathValue("domain", name)
		w := httptest.NewRecorder()
		a.adminDomainDNSPlan(w, r, actor)
		if w.Code != want {
			t.Fatalf("status=%d want %d: %s", w.Code, want, w.Body.String())
		}
		if want != http.StatusOK {
			return nil
		}
		var plan struct{ Records []mailDNSRecord }
		if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
			t.Fatal(err)
		}
		return plan.Records
	}
	defaults := request(domain, "", http.StatusOK)
	if len(defaults) != 5 || defaults[0].Type != "A" || defaults[0].Value != "" ||
		defaults[1].Value != "mail.example.test" || defaults[1].Priority != 10 || defaults[2].Value != "" {
		t.Fatalf("unexpected plan without address or relay: %+v", defaults)
	}
	records := request(domain, "?host=mail.example.test.&ipv4=203.0.113.10&ipv6=2001:db8::1&spfInclude=_spf.relay.test", http.StatusOK)
	if len(records) != 6 || records[0].Value != "203.0.113.10" || records[1].Value != "2001:db8::1" ||
		records[3].Value != "v=spf1 include:_spf.relay.test ~all" || records[4].Name != "_dmarc.example.test" {
		t.Fatalf("unexpected configured plan: %+v", records)
	}
	if _, err := db.Exec("INSERT INTO domain_dkim_keys(domain,slot,selector,public_key,sealed_key) VALUES (?,?,?,?,?)", domain, "pending", "harbor-test", "synthetic-public", []byte("synthetic-sealed")); err != nil {
		t.Fatal(err)
	}
	records = request(domain, "", http.StatusOK)
	key := records[len(records)-1]
	if key.Type != "TXT" || key.Name != "harbor-test._domainkey.example.test" || key.Value != "v=DKIM1; k=rsa; p=synthetic-public" {
		t.Fatalf("unexpected DKIM public record: %+v", key)
	}
	receiveOnly := request(domain, "?receiveOnly=1", http.StatusOK)
	if receiveOnly[2].Value != "v=spf1 -all" {
		t.Fatalf("unexpected receive-only SPF: %+v", receiveOnly[2])
	}
	request(other, "", http.StatusForbidden)
	request(domain, "?host=https://mail.example.test", http.StatusBadRequest)
	request(domain, "?ipv4=127.0.0.1", http.StatusBadRequest)
	request(domain, "?ipv6=203.0.113.10", http.StatusBadRequest)
}
