package server

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPersistentMailTranslationCache(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testPersistentMailTranslationCache(t, "sqlite", filepath.Join(t.TempDir(), "mail.sqlite"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testPersistentMailTranslationCache(t, "mysql", dsn) })
	}
}

func testPersistentMailTranslationCache(t *testing.T, driver, dsn string) {
	t.Helper()
	ctx := context.Background()
	open := func() *sql.DB {
		t.Helper()
		db, err := openDB(driver, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	a := &app{db: open(), dbDriver: driver}
	const owner, other, subject = "translation-cache@harbor.test", "translation-other@harbor.test", "Welcome"
	raw := []byte("From: support@harbor.test\r\nSubject: Welcome\r\n\r\nAn example message.\r\n")
	result, err := a.db.ExecContext(ctx, `INSERT INTO letters(owner,sender,recipient,subject,body,stamp,folder,origin,raw)
		VALUES(?,?,?,?,?,?,?,?,?)`, owner, "support@harbor.test", owner, subject, "", time.Now().Unix(), "trash", "inbox", raw)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	key := mailTranslationKey(owner, id, "microsoft", "zh-Hans", subject, raw)
	want := translatedEmail{Subject: "欢迎", Body: "这是一封示例邮件。"}
	if err := a.storeTranslation(ctx, key, subject, raw, want); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	a = &app{db: open(), dbDriver: driver}
	assertCache := func(key translationCacheKey, want translatedEmail, wantHit bool) {
		t.Helper()
		if got, hit := a.cachedTranslation(ctx, key); hit != wantHit || got != want {
			t.Fatalf("cached translation = %+v, %v; want %+v, %v", got, hit, want, wantHit)
		}
	}
	assertCount := func(want int) {
		t.Helper()
		var count int
		if err := a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mail_translations WHERE letter_id=?", id).Scan(&count); err != nil || count != want {
			t.Fatalf("stored translations = %d, %v; want %d", count, err, want)
		}
	}
	assertCache(key, want, true)
	for _, changed := range []translationCacheKey{
		{owner: other, id: id, content: key.content},
		mailTranslationKey(owner, id, "deepl", "zh-Hans", subject, raw),
		mailTranslationKey(owner, id, "microsoft", "ja", subject, raw),
		mailTranslationKey(owner, id, "microsoft", "zh-Hans", "Revised subject", raw),
		mailTranslationKey(owner, id, "microsoft", "zh-Hans", subject, []byte("another message")),
	} {
		assertCache(changed, translatedEmail{}, false)
	}
	now := time.Now().Unix()
	if _, err := a.db.ExecContext(ctx, "UPDATE mail_translations SET expires_at=? WHERE letter_id=?", now, id); err != nil {
		t.Fatal(err)
	}
	assertCache(key, translatedEmail{}, false)
	if err := a.pruneMailTranslations(ctx, now); err != nil {
		t.Fatal(err)
	}
	assertCount(0)
	for range 2 {
		if err := a.storeTranslation(ctx, key, subject, raw, want); err != nil {
			t.Fatal(err)
		}
	}
	assertCount(1)
	if _, err := a.db.ExecContext(ctx, "DELETE FROM letters WHERE id=? AND owner=?", id, owner); err != nil {
		t.Fatal(err)
	}
	assertCount(0)
	if err := a.storeTranslation(ctx, key, subject, raw, want); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("saving translation of a removed message returned %v", err)
	}
}
