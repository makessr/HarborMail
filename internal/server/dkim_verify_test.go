package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

func TestReceivedDKIMVerification(t *testing.T) {
	const domain = "mail.test"
	const sender = "sender@" + domain
	box := testDKIMCipher(t)
	selector, public, sealed, err := generateDKIMKey(box, domain)
	if err != nil {
		t.Fatal(err)
	}
	key, err := openDKIMKey(box, domain, selector, sealed)
	if err != nil {
		t.Fatal(err)
	}
	const relaySelector = "relay"
	original := []byte("From: " + sender + "\r\nTo: recipient@example.test\r\nSubject: Delivery probe\r\n\r\nOriginal body\r\n")
	header, err := signDKIMHeader(original, sender, domain, selector, key)
	if err != nil {
		t.Fatal(err)
	}
	signed := header + string(original)
	altered := strings.Replace(signed, "Original body", "Relay footnote", 1)
	relayHeader, err := signDKIMHeader([]byte(altered), sender, domain, relaySelector, key)
	if err != nil {
		t.Fatal(err)
	}
	doubleSigned := relayHeader + altered

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lookup := map[string]dkimPublicKey{}
	for _, id := range []string{selector, relaySelector} {
		record := dkimDNSKey(domain, id, public)
		lookup[record.Name+"."] = record
	}
	dnsServer := &dns.Server{PacketConn: conn, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		question := request.Question[0]
		response := new(dns.Msg).SetReply(request)
		response.Authoritative = true
		if record, ok := lookup[question.Name]; ok && question.Qtype == dns.TypeTXT {
			response.Answer = []dns.RR{&dns.TXT{Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET},
				Txt: []string{record.TXT[:200], record.TXT[200:]}}}
		} else {
			response.Rcode = dns.RcodeServerFailure
		}
		_ = w.WriteMsg(response)
	})}
	go func() { _ = dnsServer.ActivateAndServe() }()
	t.Cleanup(func() { _ = dnsServer.Shutdown(); _ = conn.Close() })
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, conn.LocalAddr().String())
	}}
	temporary, err := signDKIMHeader(original, sender, domain, "unavailable", key)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, message, status string
		args                  []string
	}{
		{"valid", signed, "pass", []string{"MAIL.TEST.", strings.ToUpper(selector)}},
		{"unsigned", string(original), "no_signature", []string{domain}},
		{"wrong domain", signed, "no_signature", []string{"unrelated.test"}},
		{"altered", altered, "fail", []string{domain, selector}},
		{"relay signature passes independently", doubleSigned, "pass", []string{domain}},
		{"relay signature cannot satisfy expected selector", doubleSigned, "fail", []string{domain, selector}},
		{"DNS server failure", temporary + string(original), "temporary_error", []string{domain}},
		{"too many signatures", strings.Repeat(header, 17) + string(original), "too_many_signatures", []string{domain}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			err := runDKIMVerify(strings.NewReader(tc.message), &output, resolver, tc.args)
			if (err == nil) != (tc.status == "pass") {
				t.Fatalf("unexpected exit result: %v", err)
			}
			var report dkimVerifyReport
			if err := json.Unmarshal(output.Bytes(), &report); err != nil || report.Status != tc.status {
				t.Fatalf("unexpected report %q: %v", output.String(), err)
			}
			if strings.Contains(output.String(), sender) || strings.Contains(output.String(), "Original body") || strings.Contains(output.String(), public) {
				t.Fatal("verification output must exclude message data and DNS key contents")
			}
		})
	}
}
