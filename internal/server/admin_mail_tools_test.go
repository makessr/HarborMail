package server

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const fixtureDMARC = `<?xml version="1.0"?>
<feedback><report_metadata><org_name>Receiver</org_name><report_id>report-123</report_id>
<date_range><begin>1787000000</begin><end>1787086400</end></date_range></report_metadata>
<policy_published><domain>example.test</domain></policy_published>
<record><row><source_ip>192.0.2.2</source_ip><count>3</count><policy_evaluated>
<disposition>none</disposition><dkim>pass</dkim><spf>fail</spf></policy_evaluated></row></record>
<record><row><source_ip>2001:db8::5</source_ip><count>2</count><policy_evaluated>
<disposition>reject</disposition><dkim>fail</dkim><spf>fail</spf></policy_evaluated></row></record></feedback>`

func TestDMARCReportFormats(t *testing.T) {
	var gz, zipped bytes.Buffer
	z := gzip.NewWriter(&gz)
	if _, err := z.Write([]byte(fixtureDMARC)); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(&zipped)
	entry, err := archive.Create("report.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(fixtureDMARC)); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	var deduplicationKey string
	for name, payload := range map[string][]byte{"xml": []byte(fixtureDMARC), "gzip": gz.Bytes(), "zip": zipped.Bytes()} {
		t.Run(name, func(t *testing.T) {
			item, sources, err := parseDMARCReport(payload)
			if err != nil || item.Domain != "example.test" || item.Aligned != 3 || item.Total != 5 ||
				len(sources) != 2 || sources[1].Disposition != "reject" {
				t.Fatalf("unexpected report: %+v %v", item, err)
			}
			if deduplicationKey != "" && item.Key != deduplicationKey {
				t.Fatal("same report has a different deduplication key")
			}
			deduplicationKey = item.Key
		})
	}
	if _, _, err := parseDMARCReport([]byte("<feedback/>")); err == nil {
		t.Fatal("incomplete document accepted")
	}
}

func TestOutboundLimitsTransactional(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testOutboundLimits(t, "sqlite", filepath.Join(t.TempDir(), "limits.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testOutboundLimits(t, "mysql", dsn) })
	}
}

func testOutboundLimits(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	domain := token()[:10] + ".test"
	owner := "account@" + domain
	if _, err := db.Exec(`INSERT INTO outbound_limits(scope,target,daily_limit,day,used)
		VALUES('account',?,5,0,0),('domain',?,3,0,0)`, owner, domain); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	try := func(count int, day time.Time) error {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := consumeOutboundLimit(ctx, tx, owner, count, day); err != nil {
			return err
		}
		return tx.Commit()
	}
	day := time.Unix(1790000000, 0)
	if err := try(2, day); err != nil {
		t.Fatal(err)
	}
	if err := try(2, day); !errors.Is(err, errOutboundLimit) {
		t.Fatalf("expected domain limit rejection, got %v", err)
	}
	var used int
	if err := db.QueryRow("SELECT used FROM outbound_limits WHERE scope='account' AND target=?", owner).Scan(&used); err != nil || used != 2 {
		t.Fatalf("rejected domain quota consumed account quota: %d, %v", used, err)
	}
	if err := try(1, day); err != nil {
		t.Fatal(err)
	}
	if err := try(3, day.Add(24*time.Hour)); err != nil {
		t.Fatalf("next day's limit was not reset: %v", err)
	}
}
