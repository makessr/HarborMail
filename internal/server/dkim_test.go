package server

import (
	"bytes"
	"crypto/cipher"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

func testDKIMCipher(t testing.TB) cipher.AEAD {
	t.Helper()
	t.Setenv("HARBOR_DKIM_KEY_FILE", "")
	t.Setenv("HARBOR_DKIM_KEY", strings.Repeat("69", 32))
	box, err := loadDKIMCipher()
	if err != nil || box == nil {
		t.Fatalf("test cipher: %v", err)
	}
	return box
}

func installTestDKIMKey(t *testing.T, a *app, domain string) (string, string) {
	t.Helper()
	selector, public, sealed, err := generateDKIMKey(a.dkimCipher, domain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO domain_dkim_keys(domain,slot,selector,public_key,sealed_key) VALUES(?,'active',?,?,?)`,
		domain, selector, public, sealed); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = a.db.Exec(`DELETE FROM domain_dkim_keys WHERE domain=?`, domain) })
	return selector, public
}

func verifyTestDKIM(t *testing.T, raw []byte, domain, selector, public string) {
	t.Helper()
	expected := dkimDNSKey(domain, selector, public)
	verified, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{LookupTXT: func(name string) ([]string, error) {
		if strings.TrimSuffix(name, ".") != expected.Name {
			return nil, fmt.Errorf("unexpected lookup %q", name)
		}
		return []string{expected.TXT}, nil
	}})
	if err != nil || len(verified) != 1 || verified[0].Err != nil || verified[0].Domain != domain {
		t.Fatalf("message failed DKIM verification: %+v %v", verified, err)
	}
}

func TestDKIMOutboxPersistence(t *testing.T) {
	for _, driver := range []string{"sqlite", "mysql"} {
		t.Run(driver, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "mail #1.db")
			if driver == "mysql" {
				source = os.Getenv("HARBOR_TEST_MYSQL_DSN")
				if source == "" {
					t.Skip("set HARBOR_TEST_MYSQL_DSN to a disposable MySQL database")
				}
			}
			db, err := openDB(driver, source)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			t.Setenv("HARBOR_DB_DRIVER", driver)
			t.Setenv("HARBOR_DB", source)
			t.Setenv("HARBOR_MYSQL_DSN", source)
			t.Setenv("HARBOR_MYSQL_DSN_FILE", "")
			a := &app{db: db, dbDriver: driver, dkimCipher: testDKIMCipher(t)}
			const domain = "dkim-fixture.test"
			const sender = "sender@" + domain
			selector, public := installTestDKIMKey(t, a, domain)
			ctx := t.Context()
			configured, err := configuredDKIMSelector(ctx, domain)
			if err != nil || configured != selector {
				t.Fatalf("DNS preflight selector: %q %v", configured, err)
			}
			raw := []byte("From: " + sender + "\r\nTo: external@elsewhere.test\r\nSubject: DKIM\r\n\r\nBody\r\n")
			m := letter{From: sender, To: "external@elsewhere.test", Subject: "DKIM", Date: time.Now().Unix()}
			m.ID, err = a.enqueue(m, raw)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM outbox WHERE id=?`, m.ID) })
			if _, err := db.Exec(`UPDATE outbox SET state='preparing',claim='first',lease=? WHERE id=?`, time.Now().Add(time.Minute).Unix(), m.ID); err != nil {
				t.Fatal(err)
			}
			job := &outboxJob{message: m, raw: raw, claim: "first"}
			if err := a.prepareDKIM(ctx, job); err != nil {
				t.Fatal(err)
			}
			var transmitted bytes.Buffer
			if err := job.writeExternalMessage(&transmitted); err != nil {
				t.Fatal(err)
			}
			verifyTestDKIM(t, transmitted.Bytes(), domain, selector, public)
			if !bytes.HasSuffix(transmitted.Bytes(), raw) {
				t.Fatal("the SMTP body or original headers were modified")
			}
			// A retry loaded by another instance must use the stored header.
			if _, err := db.Exec(`UPDATE outbox SET claim='retry' WHERE id=?`, m.ID); err != nil {
				t.Fatal(err)
			}
			retry := &outboxJob{message: m, raw: raw, claim: "retry"}
			second := &app{db: db, dbDriver: driver, dkimCipher: a.dkimCipher}
			if err := second.prepareDKIM(ctx, retry); err != nil || retry.dkimHeader != job.dkimHeader {
				t.Fatalf("cross-instance retry did not reuse signature: %v", err)
			}
			second.dkimCipher = nil
			if _, err := second.outboundDKIMHeader(ctx, retry); !errors.Is(err, errDKIMSigning) {
				t.Fatal("an instance without the secret must not relay unsigned mail")
			}
			if _, err := db.Exec(`DELETE FROM outbox WHERE id=?`, m.ID); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM outbox_dkim_signatures WHERE outbox_id=?`, m.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("cache retained after queue removal: %d %v", count, err)
			}
		})
	}
}

func BenchmarkDKIMSigning(b *testing.B) {
	box := testDKIMCipher(b)
	const domain = "harbor.test"
	const from = "sender@" + domain
	selector, _, sealed, err := generateDKIMKey(box, domain)
	if err != nil {
		b.Fatal(err)
	}
	key, err := openDKIMKey(box, domain, selector, sealed)
	if err != nil {
		b.Fatal(err)
	}
	for _, fixture := range []struct {
		name string
		size int
	}{{"16KiB", 16 << 10}, {"10MiB", 10 << 20}} {
		b.Run(fixture.name, func(b *testing.B) {
			raw := []byte("From: " + from + "\r\nTo: external@example.test\r\nSubject: DKIM fixture\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
				strings.Repeat(strings.Repeat("A", 76)+"\r\n", fixture.size/78))
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for b.Loop() {
				if _, err := signDKIMHeader(raw, from, domain, selector, key); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
