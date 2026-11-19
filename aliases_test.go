package main

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAliasConfigurationRejectsAmbiguousRoutes(t *testing.T) {
	accounts := map[string][]byte{"main@harbor.test": nil, "other@harbor.test": nil}
	for _, config := range []string{
		`{"main@harbor.test":"other@harbor.test"}`,
		`{"orders@harbor.test":"main@harbor.test","orders@harbor.test":"other@harbor.test"}`,
		`{"orders@harbor.test":"redirect@harbor.test","redirect@harbor.test":"main@harbor.test"}`,
		`{"Orders@harbor.test":"main@harbor.test"}`,
		`{"orders@harbor.test":null}`,
		`{} {}`,
	} {
		if _, err := parseAliases(config, accounts); err == nil {
			t.Fatalf("accepted invalid alias configuration %s", config)
		}
	}
	if _, err := parseAliases(`{"orders@harbor.test":"main@harbor.test"}`, accounts); err != nil {
		t.Fatal(err)
	}
}

func TestAliasDeliveryPreservesAddressAndAppliesRecipientRule(t *testing.T) {
	const owner = "main@harbor.test"
	const alias = "orders@shop.test"
	const other = "other@harbor.test"
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, accounts: map[string][]byte{owner: nil, other: nil},
		aliases: map[string]string{alias: owner}}
	if _, err := a.saveMailRule(context.Background(), owner, mailRule{
		Enabled: true, Order: 1, Field: "recipient", Value: alias, Action: "move", Destination: "archive",
	}); err != nil {
		t.Fatal(err)
	}
	signal := a.events.watch(owner)
	session := &delivery{app: a, sender: "sender@outside.test"}
	for _, recipient := range []string{strings.ToUpper(alias), alias, owner, other} {
		if err := session.Rcpt(recipient, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.Rcpt("unknown@harbor.test", nil); err == nil {
		t.Fatal("unknown address was accepted")
	}
	const raw = "To: someone@harbor.test\r\nSubject: Orders\r\n\r\nBody"
	if err := session.Data(strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-signal:
	default:
		t.Fatal("delivery did not notify the alias owner")
	}
	rows, err := db.Query(`SELECT recipient,folder,raw FROM letters WHERE owner=? ORDER BY id`, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var recipient, folder, stored string
		if err := rows.Scan(&recipient, &folder, &stored); err != nil || stored != raw {
			t.Fatalf("read routed copy: %v", err)
		}
		got = append(got, recipient+" "+folder)
	}
	if err := rows.Err(); err != nil || !slices.Equal(got, []string{alias + " archive", owner + " inbox"}) {
		t.Fatalf("unexpected alias routes %v: %v", got, err)
	}
	response := httptest.NewRecorder()
	a.addressesHTTP(response, httptest.NewRequest("GET", "/api/addresses", nil), other)
	if strings.Contains(response.Body.String(), alias) {
		t.Fatal("alias was listed for another account")
	}
}
