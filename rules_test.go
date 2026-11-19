package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestMailRulesUseEnvelopeAndRecipientPriority(t *testing.T) {
	const bob = "bob@harbor.test"
	const alice = "alice@harbor.test"
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, accounts: map[string][]byte{bob: {}, alice: {}}}
	ctx := context.Background()
	folder, err := a.saveCustomFolder(ctx, bob, "", "Invoices")
	if err != nil {
		t.Fatal(err)
	}
	save := func(owner string, rule mailRule) {
		t.Helper()
		if _, err := a.saveMailRule(ctx, owner, rule); err != nil {
			t.Fatal(err)
		}
	}
	save(bob, mailRule{Order: 5, Field: "subject", Value: "INVOICE", Action: "star"})
	save(bob, mailRule{Order: 10, Enabled: true, Field: "domain", Value: "MAIL.EXAMPLE", Action: "move", Destination: folder.ID})
	save(bob, mailRule{Order: 10, Enabled: true, Field: "subject", Value: "invoice", Action: "read"})
	save(alice, mailRule{Order: 10, Enabled: true, Field: "sender", Value: "spoof@forged.example", Action: "star"})
	save(alice, mailRule{Order: 20, Enabled: true, Field: "subject", Value: "invoice", Action: "read"})
	if err := a.deleteCustomFolder(ctx, bob, folder.ID); !errors.Is(err, errFolderUsedByRule) {
		t.Fatalf("delete referenced empty folder: %v", err)
	}
	if _, err := a.saveMailRule(ctx, alice, mailRule{Order: 10, Enabled: true, Field: "subject",
		Value: "invoice", Action: "move", Destination: folder.ID}); !errors.Is(err, errRuleFolder) {
		t.Fatalf("use another account's folder: %v", err)
	}
	raw := "From: spoof@forged.example\r\nSubject: =?UTF-8?Q?Invoice?=\r\n\r\nbody\r\n"
	delivery := &delivery{app: a, sender: "billing@mail.example",
		recipients: []mailDestination{{address: bob, owner: bob}, {address: alice, owner: alice}}}
	if err := delivery.Data(strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	for owner, want := range map[string]string{bob: folder.ID, alice: "inbox"} {
		var actual, origin string
		var read, star bool
		var stored []byte
		err := db.QueryRow(`SELECT folder,origin,seen,star,raw FROM letters WHERE owner=?`, owner).
			Scan(&actual, &origin, &read, &star, &stored)
		if err != nil || actual != want || origin != "inbox" || read != (owner == alice) || star || string(stored) != raw {
			t.Fatalf("%s: folder=%q origin=%q read=%v star=%v raw intact=%v err=%v",
				owner, actual, origin, read, star, string(stored) == raw, err)
		}
	}
}
