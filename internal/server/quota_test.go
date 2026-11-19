package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHardQuotaBackfillsAndTracksWrites(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { testHardQuota(t, "sqlite", filepath.Join(t.TempDir(), "mail.db")) })
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testHardQuota(t, "mysql", dsn) })
	}
}

func testHardQuota(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const owner = "quota-regression@harbor.test"
	insert := func(bytes int) error {
		_, err := db.Exec(`INSERT INTO letters(owner,sender,recipient,subject,body,stamp,folder,origin,raw)
			VALUES(?,'','','','',1,'inbox','inbox',?)`, owner, make([]byte, bytes))
		return err
	}
	if err := insert(800_000); err != nil {
		t.Fatal(err)
	}
	enabled, err := initHardQuota(db, driver, true)
	if err != nil || !enabled {
		t.Fatalf("quota init: enabled=%v err=%v", enabled, err)
	}
	checkUsage := func(expected int64) {
		t.Helper()
		var used int64
		if err := db.QueryRow(`SELECT used_bytes FROM mailbox_quota WHERE owner=?`, owner).Scan(&used); err != nil || used != expected {
			t.Fatalf("quota usage: got=%d expected=%d err=%v", used, expected, err)
		}
	}
	checkUsage(800_000)
	if _, err := db.Exec(`UPDATE mailbox_quota SET limit_mib=1 WHERE owner=?`, owner); err != nil {
		t.Fatal(err)
	}
	if err := insert(300_000); !isQuotaExceeded(err) {
		t.Fatalf("expected over-quota insert, got %v", err)
	}
	checkUsage(800_000)
	if _, err := db.Exec(`UPDATE letters SET raw=? WHERE owner=?`, make([]byte, 200_000), owner); err != nil {
		t.Fatal(err)
	}
	checkUsage(200_000)
	if _, err := db.Exec(`INSERT INTO outbox(sender,recipient,subject,body,stamp,raw,state,due)
		VALUES(?,'','','',1,?,'scheduled',1)`, owner, make([]byte, 700_000)); err != nil {
		t.Fatal(err)
	}
	checkUsage(900_000)
	if err := insert(150_000); !isQuotaExceeded(err) {
		t.Fatalf("expected over-quota with queue storage, got %v", err)
	}
	if _, err := db.Exec(`DELETE FROM outbox WHERE sender=?`, owner); err != nil {
		t.Fatal(err)
	}
	if err := insert(150_000); err != nil {
		t.Fatal(err)
	}
	checkUsage(350_000)
	enabled, err = initHardQuota(db, driver, false)
	if err != nil || !enabled {
		t.Fatalf("persisted quota: enabled=%v err=%v", enabled, err)
	}
	checkUsage(350_000)
}
