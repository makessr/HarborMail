package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func initContentIndex(db *sql.DB, driver string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if driver == "mysql" {
		return initMySQLContentIndex(ctx, db)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return 0, err
	}
	var parts int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name IN
		('mail_content_fts','mail_content_insert','mail_content_delete','mail_content_update')`).Scan(&parts)
	if err != nil {
		return 0, err
	}
	for _, query := range []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS mail_content_fts USING fts5(subject,body,content='letters',content_rowid='id',tokenize='trigram')`,
		`CREATE TRIGGER IF NOT EXISTS mail_content_insert AFTER INSERT ON letters BEGIN
			INSERT INTO mail_content_fts(rowid,subject,body) VALUES(NEW.id,NEW.subject,NEW.body);
		END`,
		`CREATE TRIGGER IF NOT EXISTS mail_content_delete AFTER DELETE ON letters BEGIN
			INSERT INTO mail_content_fts(mail_content_fts,rowid,subject,body) VALUES('delete',OLD.id,OLD.subject,OLD.body);
		END`,
		`CREATE TRIGGER IF NOT EXISTS mail_content_update AFTER UPDATE OF subject,body ON letters BEGIN
			INSERT INTO mail_content_fts(mail_content_fts,rowid,subject,body) VALUES('delete',OLD.id,OLD.subject,OLD.body);
			INSERT INTO mail_content_fts(rowid,subject,body) VALUES(NEW.id,NEW.subject,NEW.body);
		END`,
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return 0, err
		}
	}
	if parts != 4 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO mail_content_fts(mail_content_fts) VALUES('rebuild')`); err != nil {
			return 0, err
		}
	}
	return 3, tx.Commit()
}

func initMySQLContentIndex(ctx context.Context, db *sql.DB) (int, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var locked sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(CONCAT('harbor-fts-',LEFT(SHA2(DATABASE(),256),40)),60)`).Scan(&locked); err != nil {
		return 0, err
	}
	if locked.Int64 != 1 {
		return 0, errors.New("could not acquire the mail content index lock")
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(ctx, `SELECT RELEASE_LOCK(CONCAT('harbor-fts-',LEFT(SHA2(DATABASE(),256),40)))`)
	}()
	var exists bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.statistics
		WHERE table_schema=DATABASE() AND table_name='letters'
		AND index_name='harbor_content_fts' AND index_type='FULLTEXT')`).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE letters ADD FULLTEXT INDEX harbor_content_fts(subject,body) WITH PARSER ngram`); err != nil {
			return 0, err
		}
	}
	var tokenSize int
	err = conn.QueryRowContext(ctx, `SELECT @@ngram_token_size`).Scan(&tokenSize)
	return tokenSize, err
}

func (a *app) mailContentQuery(q string) (string, []any) {
	filter := ` AND (instr(lower(subject),lower(?))>0 OR instr(lower(body),lower(?))>0)`
	minimum := a.contentIndex
	if minimum == 0 || utf8.RuneCountInString(q) < minimum || len(q) > 512 || strings.ContainsRune(q, 0) {
		return filter, []any{q, q}
	}
	if a.dbDriver == "mysql" {
		// A Han-only phrase avoids boolean query operators and the default
		// English stopwords. Keep other queries on the exact substring path.
		for _, r := range q {
			if !unicode.Is(unicode.Han, r) {
				return filter, []any{q, q}
			}
		}
		return filter + ` AND MATCH(letters.subject,letters.body) AGAINST (? IN BOOLEAN MODE)`, []any{q, q, `"` + q + `"`}
	}
	return filter + ` AND letters.id IN (SELECT rowid FROM mail_content_fts WHERE mail_content_fts MATCH ?)`,
		[]any{q, q, `"` + strings.ReplaceAll(q, `"`, `""`) + `"`}
}
