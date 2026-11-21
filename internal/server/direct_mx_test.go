package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type testMXResolver struct {
	mx map[string][]*net.MX
	ip map[string][]netip.Addr
}

func (r testMXResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	return append([]*net.MX(nil), r.mx[name]...), nil
}

func (r testMXResolver) LookupNetIP(_ context.Context, _, name string) ([]netip.Addr, error) {
	return r.ip[name], nil
}

func fakeMXSession(conn net.Conn, final string) {
	defer conn.Close()
	_, _ = io.WriteString(conn, "220 mx.receiver.test ESMTP\r\n")
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		switch {
		case line == "EHLO mail.sender.test\r\n":
			_, _ = io.WriteString(conn, "250 mx.receiver.test\r\n")
		case strings.HasPrefix(line, "MAIL FROM:"), strings.HasPrefix(line, "RCPT TO:"):
			_, _ = io.WriteString(conn, "250 ok\r\n")
		case line == "DATA\r\n":
			_, _ = io.WriteString(conn, "354 go ahead\r\n")
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if line == ".\r\n" {
					break
				}
			}
			if final == "" {
				return
			}
			_, _ = io.WriteString(conn, final+"\r\n")
		default:
			_, _ = io.WriteString(conn, "500 unexpected fixture command\r\n")
		}
	}
}

func TestDirectMXRoutes(t *testing.T) {
	for _, tt := range []struct {
		name, domain string
		required     bool
		dials        []string
		code         int
		success      bool
	}{
		{"preferred MX unavailable", "receiver.test", false, []string{"1.1.1.1:25", "8.8.8.8:25"}, 0, true},
		{"implicit MX", "implicit.test", false, []string{"8.8.8.8:25"}, 0, true},
		{"null MX", "null.test", false, nil, 556, false},
		{"local MX beats backup", "local.test", false, nil, 550, false},
		{"private endpoints blocked", "private.test", false, nil, 0, false},
		{"TLS policy prevents cleartext", "implicit.test", true, []string{"8.8.8.8:25"}, 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var dials []string
			route := directMX{
				helo: "mail.sender.test", requireTLS: tt.required,
				resolver: testMXResolver{
					mx: map[string][]*net.MX{
						"receiver.test.": {{Host: "backup.receiver.test.", Pref: 20}, {Host: "first.receiver.test.", Pref: 10}},
						"null.test.":     {{Host: "."}},
						"local.test.":    {{Host: "mail.sender.test.", Pref: 10}, {Host: "backup.receiver.test.", Pref: 20}},
					},
					ip: map[string][]netip.Addr{
						"first.receiver.test.":  {netip.MustParseAddr("1.1.1.1")},
						"backup.receiver.test.": {netip.MustParseAddr("8.8.8.8")},
						"implicit.test.":        {netip.MustParseAddr("8.8.8.8")},
						"private.test.": {netip.MustParseAddr("::ffff:127.0.0.1"), netip.MustParseAddr("100.64.0.1"),
							netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("fe80::1")},
					},
				},
				dial: func(_ context.Context, _, destination string) (net.Conn, error) {
					dials = append(dials, destination)
					if destination == "1.1.1.1:25" {
						return nil, errors.New("fixture unavailable")
					}
					client, server := net.Pipe()
					go fakeMXSession(server, "250 accepted")
					return client, nil
				},
			}
			_, closeConn, err := route.connect(t.Context(), "person@"+tt.domain)
			if err == nil {
				defer closeConn()
			}
			if (err == nil) != tt.success || smtpResponseCode(err) != tt.code || !reflect.DeepEqual(dials, tt.dials) {
				t.Fatalf("connect: err=%v dials=%v", err, dials)
			}
		})
	}
}

func TestDirectMXOutboxAcknowledgements(t *testing.T) {
	for _, tt := range []struct{ reply, want string }{
		{"250 accepted", "sent"},
		{"450 deferred", "queued"},
		{"550 rejected", "failed"},
		{"", "uncertain"},
	} {
		t.Run(tt.want, func(t *testing.T) {
			db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			route := &directMX{
				helo:     "mail.sender.test",
				resolver: testMXResolver{ip: map[string][]netip.Addr{"receiver.test.": {netip.MustParseAddr("8.8.8.8")}}},
				dial: func(_ context.Context, _, destination string) (net.Conn, error) {
					if destination != "8.8.8.8:25" {
						return nil, fmt.Errorf("wrong destination: %s", destination)
					}
					client, server := net.Pipe()
					go fakeMXSession(server, tt.reply)
					return client, nil
				},
			}
			a := &app{db: db, dbDriver: "sqlite", domain: "mail.sender.test", directMX: route}
			m := letter{From: "sender@sender.test", To: "person@receiver.test", Subject: "Test", Date: time.Now().Unix()}
			id, err := a.enqueue(m, []byte("Subject: Test\r\n\r\nFixture"))
			if err != nil {
				t.Fatal(err)
			}
			job, err := a.claimOutbox()
			if err != nil || job == nil {
				t.Fatalf("claim: %v (%v)", job, err)
			}
			if err = a.processOutboxForRecipient(job, route.connect); err != nil {
				t.Fatal(err)
			}
			var got string
			if tt.want == "sent" {
				err = db.QueryRow("SELECT folder FROM letters WHERE owner=? AND folder='sent'", m.From).Scan(&got)
			} else {
				err = db.QueryRow("SELECT state FROM outbox WHERE id=?", id).Scan(&got)
			}
			if err != nil || got != tt.want {
				t.Fatalf("state %s, want %s (%v)", got, tt.want, err)
			}
		})
	}
}

func TestDirectMXWebmailRoutesEachRecipient(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	destinations := make(chan string, 4)
	route := &directMX{
		helo: "mail.sender.test",
		resolver: testMXResolver{ip: map[string][]netip.Addr{
			"one.test.": {netip.MustParseAddr("1.1.1.1")},
			"two.test.": {netip.MustParseAddr("8.8.8.8")},
		}},
		dial: func(ctx context.Context, _, destination string) (net.Conn, error) {
			select {
			case destinations <- destination:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			client, server := net.Pipe()
			go fakeMXSession(server, "250 accepted")
			return client, nil
		},
	}
	const sender = "sender@sender.test"
	a := &app{db: db, dbDriver: "sqlite", domain: "mail.sender.test", directMX: route, accounts: map[string][]byte{sender: nil}}
	request := httptest.NewRequest(http.MethodPost, "/api/letters", strings.NewReader(
		`{"to":"a@one.test","bcc":"b@two.test","subject":"Separate MX","body":"Fixture"}`))
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.send(w, request, sender)
	if w.Code != http.StatusAccepted {
		t.Fatalf("send returned %d: %s", w.Code, w.Body)
	}

	ctx, cancel := context.WithCancel(t.Context())
	worker := make(chan struct{})
	go func() {
		defer close(worker)
		a.runOutbox(ctx, nil)
	}()
	defer func() {
		cancel()
		<-worker
	}()
	wait := time.NewTimer(4 * time.Second)
	defer wait.Stop()
	var got []string
	for range 2 {
		select {
		case destination := <-destinations:
			got = append(got, destination)
		case <-wait.C:
			t.Fatalf("only reached %v", got)
		}
	}
	if !reflect.DeepEqual(got, []string{"1.1.1.1:25", "8.8.8.8:25"}) {
		t.Fatalf("recipient routes: %v", got)
	}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM letters WHERE owner=? AND folder='sent'", sender).Scan(&count)
		if err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		select {
		case <-tick.C:
		case <-wait.C:
			t.Fatal("worker never finalized both deliveries")
		}
	}
}

func TestLoadDirectMXDefaults(t *testing.T) {
	t.Setenv("HARBOR_DIRECT_MX_HELO", "")
	t.Setenv("HARBOR_DIRECT_MX_TLS", "")
	for _, tc := range []struct {
		name, setting, domain, wantHELO string
	}{
		{"default", "", "mail.example.test", "mail.example.test"},
		{"explicit off", "false", "mail.example.test", ""},
		{"legacy SMTP banner", "true", "localhost", "harbor.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HARBOR_DIRECT_MX", tc.setting)
			a := &app{domain: tc.domain}
			if err := a.loadDirectMX(); err != nil {
				t.Fatal(err)
			}
			if tc.wantHELO == "" {
				if a.directMX != nil {
					t.Fatal("direct MX enabled despite explicit opt-out")
				}
				return
			}
			if a.directMX == nil || a.directMX.helo != tc.wantHELO || !a.directMX.requireTLS {
				t.Fatalf("unexpected direct MX settings: %+v", a.directMX)
			}
		})
	}
}
