package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalDKIMKeyPersistsAndRequiresOriginalForExistingRecords(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "dkim-key")
	t.Setenv("HARBOR_DKIM_KEY", "")
	t.Setenv("HARBOR_DKIM_KEY_FILE", "")
	t.Setenv("HARBOR_DKIM_LOCAL_KEY_FILE", filename)
	db, err := openDB("sqlite", filepath.Join(dir, "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db}
	first, err := a.loadServerDKIM()
	if err != nil || first == nil {
		t.Fatalf("initialize local DKIM key: %v", err)
	}
	second, err := a.loadServerDKIM()
	if err != nil || second == nil {
		t.Fatalf("read persistent key: %v", err)
	}
	domain := "example.test"
	selector, public, sealed, err := generateDKIMKey(first, domain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openDKIMKey(second, domain, selector, sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO domain_dkim_keys(domain,slot,selector,public_key,sealed_key) VALUES (?,'pending',?,?,?)",
		domain, selector, public, sealed); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	if box, err := a.loadServerDKIM(); err != nil || box != nil {
		t.Fatalf("lost key must not be replaced: %v", err)
	}
	if _, err := os.Stat(filename); !os.IsNotExist(err) {
		t.Fatalf("unexpected replacement file: %v", err)
	}
}
