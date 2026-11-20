package server

import (
	"context"
	"crypto/cipher"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
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

type dkimCheckBatch struct {
	Slot    string            `json:"slot"`
	Checked int               `json:"checked"`
	Failed  int               `json:"failed"`
	Domains []dkimCheckReport `json:"domains"`
}

type dkimCheckEntry struct {
	dkimCheckReport
	public string
	sealed []byte
}

func runDKIMCheck(out io.Writer, resolver *net.Resolver, args []string) error {
	slot := "active"
	if len(args) == 2 && args[1] == "--pending" {
		slot = "pending"
	} else if len(args) != 1 {
		return errors.New("usage: harbormail dkim-check DOMAIN|--all [--pending]")
	}
	all := args[0] == "--all"
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(args[0])), ".")
	if !all && (!strings.Contains(domain, ".") || !validDNSName(domain) || len("harbor-0000000000000000._domainkey."+domain) > 253) {
		return errors.New("provide a fully qualified mail domain; use Punycode for internationalized domains")
	}
	db, err := openConfiguredCheckDB()
	if err != nil {
		return err
	}
	defer db.Close()
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	box, secretErr := loadDKIMCipher()
	if all {
		batch, err := inspectAllDKIM(db, resolver, box, secretErr, slot)
		if err != nil {
			return err
		}
		if err := encoder.Encode(batch); err != nil {
			return err
		}
		if batch.Checked == 0 {
			return fmt.Errorf("no %s DKIM keys to check", slot)
		}
		if batch.Failed > 0 {
			return fmt.Errorf("DKIM preflight failed for %d of %d domains", batch.Failed, batch.Checked)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	report, err := inspectDKIM(ctx, db, resolver, box, secretErr, domain, slot)
	if err != nil {
		return err
	}
	if err := encoder.Encode(report); err != nil {
		return err
	}
	if report.KeyState != "match" || report.DNSState != "match" {
		return errors.New("DKIM preflight failed; see keyState and dnsState")
	}
	return nil
}

func inspectDKIM(ctx context.Context, db folderQueryer, resolver *net.Resolver, box cipher.AEAD, secretErr error, domain, slot string) (dkimCheckReport, error) {
	entry := dkimCheckEntry{dkimCheckReport: dkimCheckReport{Domain: domain, Slot: slot, KeyState: "not_configured", DNSState: "skipped"}}
	err := db.QueryRowContext(ctx, `SELECT selector,public_key,sealed_key FROM domain_dkim_keys WHERE domain=? AND slot=?`,
		domain, slot).Scan(&entry.Selector, &entry.public, &entry.sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return entry.dkimCheckReport, nil
	}
	if err != nil {
		return dkimCheckReport{}, errors.New("could not read DKIM settings from the configured database")
	}
	return entry.evaluate(ctx, resolver, box, secretErr), nil
}

func readDKIMCheckEntries(ctx context.Context, db *sql.DB, slot string) ([]dkimCheckEntry, error) {
	rows, err := db.QueryContext(ctx, `SELECT domain,selector,public_key,sealed_key FROM domain_dkim_keys WHERE slot=? ORDER BY domain`, slot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []dkimCheckEntry
	for rows.Next() {
		entry := dkimCheckEntry{dkimCheckReport: dkimCheckReport{Slot: slot}}
		if err := rows.Scan(&entry.Domain, &entry.Selector, &entry.public, &entry.sealed); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func inspectAllDKIM(db *sql.DB, resolver *net.Resolver, box cipher.AEAD, secretErr error, slot string) (dkimCheckBatch, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	entries, err := readDKIMCheckEntries(ctx, db, slot)
	cancel()
	if err != nil {
		return dkimCheckBatch{}, errors.New("could not read DKIM settings from the configured database")
	}
	batch := dkimCheckBatch{Slot: slot, Checked: len(entries), Domains: make([]dkimCheckReport, len(entries))}
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(4, len(entries)) {
		workers.Go(func() {
			for index := range jobs {
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				batch.Domains[index] = entries[index].evaluate(ctx, resolver, box, secretErr)
				cancel()
			}
		})
	}
	for index := range entries {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	for _, report := range batch.Domains {
		if report.KeyState != "match" || report.DNSState != "match" {
			batch.Failed++
		}
	}
	return batch, nil
}

func (entry dkimCheckEntry) evaluate(ctx context.Context, resolver *net.Resolver, box cipher.AEAD, secretErr error) dkimCheckReport {
	report := entry.dkimCheckReport
	report.Record = dkimDNSKey(report.Domain, report.Selector, entry.public).Name
	if secretErr != nil {
		report.KeyState = "secret_unusable"
	} else {
		report.KeyState = storedDKIMKeyState(box, report.Domain, report.Selector, entry.public, entry.sealed)
	}
	report.DNSState = checkDKIMPublicKey(ctx, resolver, report.Record, entry.public)
	return report
}

func storedDKIMKeyState(box cipher.AEAD, domain, selector, public string, sealed []byte) string {
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
