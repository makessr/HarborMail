package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestDatabaseMixedLoad(t *testing.T) {
	if os.Getenv("HARBOR_DATABASE_LOAD") != "1" {
		t.Skip("set HARBOR_DATABASE_LOAD=1 to run the synthetic load test")
	}
	t.Run("sqlite", func(t *testing.T) { databaseMixedLoad(t, "sqlite", filepath.Join(t.TempDir(), "load.db")) })
	dsn := os.Getenv("HARBOR_DATABASE_LOAD_MYSQL_DSN")
	if dsn == "" {
		return
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil || !strings.HasPrefix(config.DBName, "harbor_load_") {
		t.Fatal("use a disposable MySQL database named harbor_load_*")
	}
	t.Run("mysql", func(t *testing.T) { databaseMixedLoad(t, "mysql", dsn) })
}

func databaseMixedLoad(t *testing.T, driver, source string) {
	t.Helper()
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const seedCount = 12000
	owner := "load-" + token()[:8] + "@harbor.test"
	seedDatabaseSearch(t, db, owner, seedCount)
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%t", indexed), func(t *testing.T) {
			a := &app{db: db, dbDriver: driver, accounts: map[string][]byte{owner: nil}}
			if indexed {
				size, err := initContentIndex(db, driver)
				if err != nil {
					t.Fatal(err)
				}
				a.contentIndex = size
			}
			runDatabaseMixedLoad(t, a, owner, seedCount/1000)
		})
	}
}

func seedDatabaseSearch(t *testing.T, db *sql.DB, owner string, count int) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(t.Context(), `INSERT INTO letters(owner,sender,recipient,subject,body,stamp,folder,origin,raw)
		VALUES(?,'sender@example.test',?,'Synthetic seed',?,1,'inbox','inbox',?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	filler := strings.Repeat("这是普通的历史邮件。 Standard mailbox archive body.\n", 40)
	for i := range count {
		body := filler
		if i%1000 == 0 {
			body += "年度报告 Harbor Invoice"
		}
		if _, err := stmt.ExecContext(t.Context(), owner, owner, body, []byte{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func runDatabaseMixedLoad(t *testing.T, a *app, owner string, expectedMatches int) {
	t.Helper()
	const iterations = 25
	type sample struct {
		kind string
		time time.Duration
		err  error
	}
	start := make(chan struct{})
	results := make(chan sample, 12*iterations)
	var running sync.WaitGroup
	for i := range 12 {
		running.Go(func() {
			<-start
			for j := range iterations {
				begin := time.Now()
				kind := "smtp-ingest"
				var err error
				if i < 4 {
					s := &delivery{app: a, sender: "sender@example.test", recipients: []mailDestination{{address: owner, owner: owner}}}
					err = s.Data(strings.NewReader(fmt.Sprintf("Subject: Load %d-%d\r\n\r\nSynthetic incoming mail.\r\n", i, j)))
				} else {
					kind = "年度报告"
					if i >= 8 {
						kind = "报告 Harbor"
					}
					err = verifyLoadSearch(t, a, owner, kind, expectedMatches)
				}
				results <- sample{kind, time.Since(begin), err}
			}
		})
	}
	stats := a.db.Stats()
	lockWaits := loadRowLockWaits(a)
	var before, received int
	const countReceived = `SELECT COUNT(*) FROM letters WHERE owner=? AND subject LIKE 'Load %'`
	if err := a.db.QueryRow(countReceived, owner).Scan(&before); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	close(start)
	running.Wait()
	close(results)
	latencies := make(map[string][]time.Duration)
	for result := range results {
		if result.err != nil {
			t.Errorf("%s: %v", result.kind, result.err)
		}
		latencies[result.kind] = append(latencies[result.kind], result.time)
	}
	for _, kind := range []string{"smtp-ingest", "年度报告", "报告 Harbor"} {
		values := latencies[kind]
		slices.Sort(values)
		t.Logf("%s n=%d p50=%v p95=%v max=%v", kind, len(values), values[len(values)/2-1], values[len(values)*95/100-1], values[len(values)-1])
	}
	if err := a.db.QueryRow(countReceived, owner).Scan(&received); err != nil || received-before != 4*iterations {
		t.Fatalf("stored %d new messages: %v", received-before, err)
	}
	t.Logf("elapsed=%v db_pool_waits=%d pool_wait_time=%v mysql_row_lock_waits=%d stored=%d",
		time.Since(started), a.db.Stats().WaitCount-stats.WaitCount, a.db.Stats().WaitDuration-stats.WaitDuration, loadRowLockWaits(a)-lockWaits, received-before)
}

func verifyLoadSearch(t *testing.T, a *app, owner, keyword string, want int) error {
	request := httptest.NewRequest("GET", "/api/letters?folder=inbox&q="+url.QueryEscape(keyword), nil).WithContext(t.Context())
	response := httptest.NewRecorder()
	a.list(response, request, owner)
	var result struct{ Letters []letter }
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		return err
	}
	if response.Code != 200 || len(result.Letters) != want {
		return fmt.Errorf("HTTP %d with %d matches, want %d", response.Code, len(result.Letters), want)
	}
	return nil
}

func loadRowLockWaits(a *app) int64 {
	var name string
	var waits int64
	if a.dbDriver == "mysql" {
		_ = a.db.QueryRow("SHOW GLOBAL STATUS LIKE 'Innodb_row_lock_waits'").Scan(&name, &waits)
	}
	return waits
}
