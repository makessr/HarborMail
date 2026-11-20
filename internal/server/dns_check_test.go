package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"testing"

	"github.com/miekg/dns"
)

func TestDomainDNSCheck(t *testing.T) {
	records := map[string][]dns.RR{
		"mail.test.": {
			&dns.MX{Hdr: dns.RR_Header{Name: "mail.test.", Rrtype: dns.TypeMX, Class: dns.ClassINET}, Preference: 10, Mx: "mx.mail.test."},
			&dns.TXT{Hdr: dns.RR_Header{Name: "mail.test.", Rrtype: dns.TypeTXT, Class: dns.ClassINET}, Txt: []string{"v=spf1 -all"}},
		},
		"_dmarc.mail.test.": {
			&dns.TXT{Hdr: dns.RR_Header{Name: "_dmarc.mail.test.", Rrtype: dns.TypeTXT, Class: dns.ClassINET}, Txt: []string{"v=DMARC1; p=none"}},
		},
		"s1._domainkey.mail.test.": {
			&dns.TXT{Hdr: dns.RR_Header{Name: "s1._domainkey.mail.test.", Rrtype: dns.TypeTXT, Class: dns.ClassINET}, Txt: []string{"v=DKIM1; p=AAAA"}},
		},
		"closed.test.": {
			&dns.MX{Hdr: dns.RR_Header{Name: "closed.test.", Rrtype: dns.TypeMX, Class: dns.ClassINET}, Mx: "."},
			&dns.TXT{Hdr: dns.RR_Header{Name: "closed.test.", Rrtype: dns.TypeTXT, Class: dns.ClassINET}, Txt: []string{"v=spf1 -all"}},
			&dns.TXT{Hdr: dns.RR_Header{Name: "closed.test.", Rrtype: dns.TypeTXT, Class: dns.ClassINET}, Txt: []string{"v=spf1 ?all"}},
		},
	}
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{PacketConn: conn, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		question := request.Question[0]
		response := new(dns.Msg).SetReply(request)
		if question.Name == "_dmarc.closed.test." {
			response.Rcode = dns.RcodeServerFailure
		} else if answers := records[question.Name]; answers != nil {
			for _, answer := range answers {
				if answer.Header().Rrtype == question.Qtype {
					response.Answer = append(response.Answer, answer)
				}
			}
		} else {
			response.Rcode = dns.RcodeNameError
		}
		_ = w.WriteMsg(response)
	})}
	go func() { _ = server.ActivateAndServe() }()
	t.Cleanup(func() { _ = server.Shutdown(); _ = conn.Close() })
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, conn.LocalAddr().String())
	}}

	for _, tc := range []struct {
		name                 string
		args                 []string
		mx, spf, dmarc, dkim string
		success              bool
	}{
		{"present", []string{"MAIL.TEST.", "S1"}, "present", "present", "present", "present", true},
		{"no selector", []string{"mail.test"}, "present", "present", "present", "skipped", true},
		{"record issues", []string{"closed.test", "s1"}, "null", "multiple", "error", "missing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := runDomainDNSCheck(&out, resolver, tc.args); (err == nil) != tc.success {
				t.Fatalf("unexpected command result: %v", err)
			}
			var report domainDNS
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if report.MX.State != tc.mx || report.SPF.State != tc.spf || report.DMARC.State != tc.dmarc || report.DKIM.State != tc.dkim {
				t.Fatalf("unexpected DNS report: %s", out.String())
			}
			if tc.success && (len(report.Hosts) != 1 || report.Hosts[0].Host != "mx.mail.test." || report.Hosts[0].Pref != 10) {
				t.Fatalf("unexpected MX hosts: %s", out.String())
			}
		})
	}
	var out bytes.Buffer
	if err := runDomainDNSCheck(&out, nil, []string{"mail.test", "bad selector"}); err == nil || out.Len() != 0 {
		t.Fatal("expected invalid input to be rejected before DNS lookup")
	}
}
