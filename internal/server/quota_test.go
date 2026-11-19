package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCancelQueuedMailAfterQuotaReduction(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testCancelQueuedMailAfterQuotaReduction(t, "sqlite", filepath.Join(t.TempDir(), "mail.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testCancelQueuedMailAfterQuotaReduction(t, "mysql", dsn) })
	}
}

func testCancelQueuedMailAfterQuotaReduction(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	enabled, err := initHardQuota(db, driver, true)
	if err != nil || !enabled {
		t.Fatalf("quota setup: enabled=%v (%v)", enabled, err)
	}
	owner := token()[:16] + "@harbor.test"
	const recipient = "recipient@harbor.test"
	a := &app{db: db, domain: "harbor.test", hardQuota: enabled,
		accounts: map[string][]byte{owner: nil, recipient: nil}}
	if _, err := db.Exec(`INSERT INTO letters(owner,sender,recipient,subject,body,stamp,folder,origin,raw)
		VALUES(?,'','','','',1,'inbox','inbox',?)`, owner, make([]byte, 2<<20)); err != nil {
		t.Fatal(err)
	}
	var queued []int64
	for _, query := range []string{"undoSeconds=30", fmt.Sprintf("sendAt=%d", time.Now().Add(time.Hour).Unix())} {
		r := httptest.NewRequest(http.MethodPost, "/api/letters?"+query, strings.NewReader(
			`{"bcc":"recipient@harbor.test","subject":"Quota test","body":"Synthetic body"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		a.send(w, r, owner)
		var result struct{ Queued int64 }
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusAccepted || result.Queued == 0 {
			t.Fatalf("queue email: HTTP %d, %s (%v)", w.Code, w.Body, err)
		}
		queued = append(queued, result.Queued)
	}
	if _, err := db.Exec(`UPDATE mailbox_quota SET limit_mib=1 WHERE owner=?`, owner); err != nil {
		t.Fatal(err)
	}
	for _, id := range queued {
		r := httptest.NewRequest(http.MethodPost, "/api/outbox/id/cancel", nil)
		r.SetPathValue("id", fmt.Sprint(id))
		w := httptest.NewRecorder()
		a.cancelScheduled(w, r, owner)
		var result struct{ Draft int64 }
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusOK || result.Draft == 0 {
			t.Fatalf("cancel accepted email %d: HTTP %d, %s (%v)", id, w.Code, w.Body, err)
		}
		var folder, bcc string
		if err := db.QueryRow(`SELECT l.folder,r.bcc_list FROM letters l JOIN web_letter_recipients r
			ON r.letter_id=l.id WHERE l.owner=? AND l.id=?`, owner, result.Draft).Scan(&folder, &bcc); err != nil ||
			folder != "drafts" || bcc != recipient {
			t.Fatalf("restored draft: folder=%q bcc=%q (%v)", folder, bcc, err)
		}
	}
	var pending int
	if err := db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE sender=?`, owner).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("canceled tasks remain in the queue: %d (%v)", pending, err)
	}
	var used, limit, actual int64
	if err := db.QueryRow(`SELECT used_bytes,limit_mib,(SELECT SUM(LENGTH(raw)) FROM letters WHERE owner=?)
		FROM mailbox_quota WHERE owner=?`, owner, owner).Scan(&used, &limit, &actual); err != nil || limit != 1 || used != actual {
		t.Fatalf("quota after cancel: used=%d limit=%d actual=%d (%v)", used, limit, actual, err)
	}
	if _, err := db.Exec(`INSERT INTO letters(owner,sender,recipient,subject,body,stamp,folder,origin,raw)
		VALUES(?,'','','','',1,'inbox','inbox',?)`, owner, []byte("new message")); !isQuotaExceeded(err) {
		t.Fatalf("new mail was not subject to the reduced quota: %v", err)
	}
}

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

	if _, err := db.Exec(`DROP TRIGGER quota_letters_insert`); err != nil {
		t.Fatal(err)
	}
	if err := insert(150_000); err != nil {
		t.Fatal(err)
	}
	checkUsage(350_000)
	enabled, err = initHardQuota(db, driver, false)
	if err != nil || !enabled {
		t.Fatalf("quota repair: enabled=%v err=%v", enabled, err)
	}
	checkUsage(500_000)
	if err := insert(600_000); !isQuotaExceeded(err) {
		t.Fatalf("expected repaired quota trigger to enforce existing limit, got %v", err)
	}
	checkUsage(500_000)
}
