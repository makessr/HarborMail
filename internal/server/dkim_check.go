package server

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"time"
)

type dkimCheckReport struct {
	Domain   string `json:"domain"`
	Slot     string `json:"slot"`
	Selector string `json:"selector,omitempty"`
	Record   string `json:"record,omitempty"`
	KeyState string `json:"keyState"`
	DNSState string `json:"dnsState"`
}

func runDKIMCheck(out io.Writer, resolver *net.Resolver, args []string) error {
	slot := "active"
	if len(args) == 2 && args[1] == "--pending" {
		slot = "pending"
	} else if len(args) != 1 {
		return errors.New("usage: harbormail dkim-check DOMAIN [--pending]")
	}
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(args[0])), ".")
	if !strings.Contains(domain, ".") || !validDNSName(domain) || len("harbor-0000000000000000._domainkey."+domain) > 253 {
		return errors.New("provide a fully qualified mail domain; use Punycode for internationalized domains")
	}
	db, err := openConfiguredCheckDB()
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	report, err := inspectDKIM(ctx, db, resolver, domain, slot)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return err
	}
	if report.KeyState != "match" || report.DNSState != "match" {
		return errors.New("DKIM preflight failed; see keyState and dnsState")
	}
	return nil
}

func inspectDKIM(ctx context.Context, db folderQueryer, resolver *net.Resolver, domain, slot string) (dkimCheckReport, error) {
	report := dkimCheckReport{Domain: domain, Slot: slot, KeyState: "not_configured", DNSState: "skipped"}
	var public string
	var sealed []byte
	err := db.QueryRowContext(ctx, `SELECT selector,public_key,sealed_key FROM domain_dkim_keys WHERE domain=? AND slot=?`,
		domain, slot).Scan(&report.Selector, &public, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return report, nil
	}
	if err != nil {
		return report, errors.New("could not read DKIM settings from the configured database")
	}
	report.Record = dkimDNSKey(domain, report.Selector, public).Name
	report.KeyState = checkDKIMPrivateKey(domain, report.Selector, public, sealed)
	report.DNSState = checkDKIMPublicKey(ctx, resolver, report.Record, public)
	return report, nil
}

func checkDKIMPrivateKey(domain, selector, public string, sealed []byte) string {
	box, err := loadDKIMCipher()
	if err != nil {
		return "secret_unusable"
	}
	if box == nil {
		return "secret_missing"
	}
	key, err := openDKIMKey(box, domain, selector, sealed)
	if err != nil {
		return "decrypt_failed"
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil || base64.StdEncoding.EncodeToString(publicDER) != public {
		return "mismatch"
	}
	return "match"
}
