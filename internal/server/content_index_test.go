package server

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContentIndexBackfillUpdatesAndAccountScope(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { testContentIndex(t, "sqlite", filepath.Join(t.TempDir(), "mail.db")) })
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testContentIndex(t, "mysql", dsn) })
	}
}

func BenchmarkGeneralMailSearch(b *testing.B) {
	db, err := openDB("sqlite", filepath.Join(b.TempDir(), "search.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(b.Context(), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	insert, err := tx.Prepare(`INSERT INTO letters(owner,sender,recipient,subject,body,stamp,folder,origin,raw)
		VALUES('owner@harbor.test','source@harbor.test','owner@harbor.test','主题',?,1,'inbox','inbox',X'')`)
	if err != nil {
		b.Fatal(err)
	}
	filler := strings.Repeat("ordinary message text for mailbox search. ", 180)
	for i := range 3000 {
		body := filler
		if i%200 == 0 {
			body += "年度报告"
		}
		if _, err := insert.Exec(body); err != nil {
			b.Fatal(err)
		}
	}
	insert.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	size, err := initContentIndex(db, "sqlite")
	if err != nil {
		b.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		size int
	}{{"scan", 0}, {"indexed", size}} {
		b.Run(tc.name, func(b *testing.B) {
			a := &app{dbDriver: "sqlite", contentIndex: tc.size}
			const owner = "owner@harbor.test"
			filter, args := a.mailGeneralQuery(owner, "年度报告")
			args = append([]any{owner}, args...)
			b.ReportAllocs()
			for b.Loop() {
				var count int
				err := db.QueryRow(`SELECT COUNT(*) FROM letters LEFT JOIN web_letter_recipients e ON e.letter_id=letters.id
					WHERE letters.owner=? AND folder='inbox'`+filter, args...).Scan(&count)
				if err != nil || count != 15 {
					b.Fatalf("count=%d, err=%v", count, err)
				}
			}
		})
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
	general := func(query string, want int) {
		t.Helper()
		filter, args := a.mailGeneralQuery("one@harbor.test", query)
		var count int
		err := db.QueryRow(`SELECT COUNT(*) FROM letters LEFT JOIN web_letter_recipients e ON e.letter_id=letters.id
			WHERE letters.owner=?`+filter, append([]any{"one@harbor.test"}, args...)...).Scan(&count)
		if err != nil || count != want {
			t.Fatalf("general search %q: count=%d want=%d err=%v", query, count, want, err)
		}
	}
	general("年度报告", 1)
	general("主题 年度", 1)
	if _, err := db.Exec(`UPDATE letters SET sender=? WHERE id=?`, "项目联络 <hello@harbor.test>", first); err != nil {
		t.Fatal(err)
	}
	general("项目联络", 1)
	insert(second, "two@harbor.test", "年度报告初稿")
	match("年度报告", 1)
	if _, err := db.Exec(`UPDATE letters SET body=? WHERE id=?`, "终稿已经归档", first); err != nil {
		t.Fatal(err)
	}
	match("年度报告", 0)
	general("年度报告", 0)
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
