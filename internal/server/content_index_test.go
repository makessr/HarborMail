package server

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestContentIndexBackfillUpdatesAndAccountScope(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { testContentIndex(t, "sqlite", filepath.Join(t.TempDir(), "mail.db")) })
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testContentIndex(t, "mysql", dsn) })
	}
}

func testContentIndex(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	insert := func(id int, owner, body string) {
		t.Helper()
		_, err := db.Exec(`INSERT INTO letters(id,owner,sender,recipient,subject,body,stamp,folder,origin,raw)
			VALUES(?,?,'','','旧主题',?,1,'inbox','inbox',?)`, id, owner, body, []byte{})
		if err != nil {
			t.Fatal(err)
		}
	}
	const first, second = 910021, 910022
	insert(first, "one@harbor.test", "年度报告初稿")
	size, err := initContentIndex(db, driver)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, dbDriver: driver, contentIndex: size}
	match := func(query string, want int) {
		t.Helper()
		filter, args := a.mailContentQuery(query)
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM letters WHERE owner=?"+filter, append([]any{"one@harbor.test"}, args...)...).Scan(&count)
		if err != nil || count != want {
			t.Fatalf("%q: count=%d want=%d err=%v", query, count, want, err)
		}
	}
	match("年度报告", 1)
	match("报告", 1)
	insert(second, "two@harbor.test", "年度报告初稿")
	match("年度报告", 1)
	if _, err := db.Exec(`UPDATE letters SET body=? WHERE id=?`, "终稿已经归档", first); err != nil {
		t.Fatal(err)
	}
	match("年度报告", 0)
	match("已经归档", 1)
	if _, err := db.Exec(`DELETE FROM letters WHERE id=?`, first); err != nil {
		t.Fatal(err)
	}
	match("已经归档", 0)
	if driver == "mysql" {
		return
	}
	var row int
	if err := db.QueryRow(`SELECT rowid FROM mail_content_fts WHERE mail_content_fts MATCH '"已经归档"'`).Scan(&row); err != sql.ErrNoRows {
		t.Fatalf("deleted document remains indexed: id=%d err=%v", row, err)
	}
}
