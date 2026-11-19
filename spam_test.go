package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSpamScannerQuarantinesAndRespectsRecipientRule(t *testing.T) {
	const one = "first@harbor.test"
	const two = "second@harbor.test"
	const raw = "Subject: Example\r\nX-Spam: Yes\r\n\r\nBody\r\n"
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != raw || r.URL.Path != "/checkv2" || r.Method != http.MethodPost {
			t.Error("worker did not receive the original RFC 822 message")
		}
		if r.Header.Get("From") != "sender@example.test" || !slices.Equal(r.Header.Values("Rcpt"), []string{one, two}) {
			t.Error("worker did not receive the SMTP envelope")
		}
		if _, err := w.Write([]byte(`{"action":"reject"}`)); err != nil {
			t.Error(err)
		}
	}))
	defer worker.Close()
	scanner, err := newRspamdScanner(worker.URL + "/checkv2")
	if err != nil {
		t.Fatal(err)
	}
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, accounts: map[string][]byte{one: {}, two: {}}, spamScanner: scanner}
	_, err = a.saveMailRule(context.Background(), two, mailRule{
		Order: 1, Enabled: true, Field: "domain", Value: "example.test", Action: "move", Destination: "inbox",
	})
	if err != nil {
		t.Fatal(err)
	}
	session := &delivery{app: a, sender: "sender@example.test",
		recipients: []mailDestination{{address: one, owner: one}, {address: two, owner: two}}}
	if err := session.Data(strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	worker.Close()
	if err := session.Data(strings.NewReader(raw)); err != nil {
		t.Fatalf("deliver when scanner is unavailable: %v", err)
	}
	for owner, want := range map[string][]string{one: {"junk", "inbox"}, two: {"inbox", "inbox"}} {
		rows, err := db.Query(`SELECT folder,raw FROM letters WHERE owner=? ORDER BY id`, owner)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var folder string
			var stored []byte
			if err := rows.Scan(&folder, &stored); err != nil || string(stored) != raw {
				rows.Close()
				t.Fatalf("scan %s: original message changed or unavailable: %v", owner, err)
			}
			got = append(got, folder)
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("%s: folders=%v, want %v (err=%v)", owner, got, want, err)
		}
	}
}

func TestLegacyCustomJunkRetainsItsUIDs(t *testing.T) {
	const owner = "old@harbor.test"
	const folder = "u000000000000001"
	path := filepath.Join(t.TempDir(), "mail.db")
	db, err := openDB("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Seed an account using a name that was valid before Junk became reserved.
	if _, err := db.Exec(`INSERT INTO custom_folders(folder,owner,label,lookup) VALUES
		(?,?,'Junk','junk'),('u000000000000002',?,'Junk (自定义)','junk (自定义)')`, folder, owner, owner); err != nil {
		t.Fatal(err)
	}
	a := &app{db: db}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := insert(tx, owner, letter{From: "sender@example.test", To: owner}, folder, []byte("Subject: Keep\r\n\r\nBody")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	before, validity, _, err := a.imapSnapshot(owner, folder)
	if err != nil || len(before) != 1 {
		t.Fatalf("legacy mailbox has %d messages: %v", len(before), err)
	}
	if err := renameLegacyJunkFolders(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	user := imapUser{app: a, owner: owner}
	builtin, err := user.GetMailbox("Junk")
	if err != nil || builtin.(*imapMailbox).folder != "junk" {
		t.Fatalf("system Junk missing: %v", err)
	}
	old, err := user.GetMailbox("Junk (自定义 2)")
	if err != nil || old.(*imapMailbox).folder != folder {
		t.Fatalf("custom Junk missing: %v", err)
	}
	after, updatedValidity, _, err := a.imapSnapshot(owner, folder)
	if err != nil || len(after) != 1 || validity != updatedValidity || before[0].Uid != after[0].Uid {
		t.Fatalf("renaming changed existing UID or UIDVALIDITY: %v", err)
	}
}
