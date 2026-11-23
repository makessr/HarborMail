package server

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoginInfrastructureAndRateLimitResponses(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		a       *app
		limited bool
		want    int
	}{
		{"credential lookup unavailable", &app{db: db}, false, http.StatusServiceUnavailable},
		{"shared limiter unavailable", &app{db: db, webLogin: webLoginGuard{shared: &mysqlLoginLimiter{db: db}}}, false, http.StatusServiceUnavailable},
		{"local rate limit", &app{db: db}, true, http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.limited {
				source := netip.MustParsePrefix("192.0.2.20/32")
				for range loginSourceLimit {
					tc.a.webLogin.allow(time.Now(), source, "", false)
				}
			}
			req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"email":"mail@example.test","password":"invalid"}`))
			req.RemoteAddr = "192.0.2.20:12345"
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			tc.a.routes().ServeHTTP(recorder, req)
			if recorder.Code != tc.want || (recorder.Header().Get("Retry-After") != "") != tc.limited {
				t.Fatalf("status=%d retry=%q body=%s", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body)
			}
		})
	}
}

func TestExistingSQLiteAuditIndex(t *testing.T) {
	source := filepath.Join(t.TempDir(), "existing.db")
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE admin_alias_actions(id INTEGER PRIMARY KEY AUTOINCREMENT,
		actor TEXT NOT NULL, target TEXT NOT NULL, alias TEXT NOT NULL, action VARCHAR(24) NOT NULL, stamp INTEGER NOT NULL);
		INSERT INTO admin_alias_actions(actor,target,alias,action,stamp) VALUES('owner','target','alias','alias_add',123)`)
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("legacy fixture: %v, %v", err, closeErr)
	}
	db, err = openDB("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var columns string
	var records int
	if err := db.QueryRow(`SELECT group_concat(name,',') FROM (SELECT name FROM pragma_index_info('admin_alias_action_time') ORDER BY seqno)`).Scan(&columns); err != nil || columns != "stamp,id" {
		t.Fatalf("index columns: %q, %v", columns, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM admin_alias_actions WHERE stamp=123`).Scan(&records); err != nil || records != 1 {
		t.Fatalf("legacy records: %d, %v", records, err)
	}
}

func TestSQLiteMySQLMigrationSchemaParity(t *testing.T) {
	dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("requires a disposable MySQL database")
	}
	sqlite, err := openDB("sqlite", filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlite.Close()
	mysql, err := openDB("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer mysql.Close()
	ctx := t.Context()
	tx, err := sqlite.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	tables, err := readMigrationTables(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		var columns string
		err := mysql.QueryRowContext(ctx, `SELECT GROUP_CONCAT(column_name ORDER BY ordinal_position)
			FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=?`, table.name).Scan(&columns)
		if err != nil {
			t.Errorf("%s schema: %v", table.name, err)
			continue
		}
		mysqlColumns := "," + columns + ","
		for _, column := range table.columns {
			if !strings.Contains(mysqlColumns, ","+strings.Trim(column, "`")+",") {
				t.Errorf("missing MySQL migration column %s.%s", table.name, column)
			}
		}
	}
	var index string
	if err := mysql.QueryRowContext(ctx, `SELECT GROUP_CONCAT(column_name ORDER BY seq_in_index)
		FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='admin_alias_actions'
		AND index_name='admin_alias_action_time'`).Scan(&index); err != nil || index != "stamp,id" {
		t.Fatalf("MySQL audit index: %q, %v", index, err)
	}
}

func TestPublicMailboxDefaultsPreserveOverrides(t *testing.T) {
	test := func(t *testing.T, driver, source string) {
		db, err := openDB(driver, source)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		a := &app{db: db, dbDriver: driver, publicDefaults: true}
		owner := token()[:12] + "@" + token()[:12] + ".test"
		seed := func() {
			t.Helper()
			tx, err := db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := a.seedMailboxLimits(t.Context(), tx, owner); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		seed()
		var quota, limit, domain int
		read := func() {
			t.Helper()
			if err := db.QueryRow("SELECT limit_mib FROM mailbox_quota WHERE owner=?", owner).Scan(&quota); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT daily_limit FROM outbound_limits WHERE scope='account' AND target=?", owner).Scan(&limit); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT daily_limit FROM outbound_limits WHERE scope='domain' AND target=?", emailDomain(owner)).Scan(&domain); err != nil {
				t.Fatal(err)
			}
		}
		read()
		if quota != publicMailboxQuotaMiB || limit != publicAccountDailyRecipients || domain != publicDomainDailyRecipients {
			t.Fatalf("unexpected defaults: %d, %d, %d", quota, limit, domain)
		}
		for _, statement := range []string{"UPDATE mailbox_quota SET limit_mib=0 WHERE owner=?",
			"UPDATE outbound_limits SET daily_limit=0 WHERE scope='account' AND target=?"} {
			if _, err := db.Exec(statement, owner); err != nil {
				t.Fatal(err)
			}
		}
		seed()
		read()
		if quota != 0 || limit != 0 || domain != publicDomainDailyRecipients {
			t.Fatalf("overrides were lost: %d, %d, %d", quota, limit, domain)
		}
	}
	t.Run("sqlite", func(t *testing.T) { test(t, "sqlite", filepath.Join(t.TempDir(), "mail.db")) })
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { test(t, "mysql", dsn) })
	}
}
