package server

import (
	"context"
	"crypto/sha256"
	"log"
	"time"
)

const (
	translationCacheTTL      = 30 * 24 * time.Hour
	maxTranslationCacheEntry = 128 << 10
)

type emailTranslation struct {
	providers map[string]emailTranslator
	slots     chan struct{}
}

func (t *emailTranslation) providerIDs() []string {
	if t == nil {
		return nil
	}
	var ids []string
	for _, id := range []string{"microsoft", "deepl", "google", "libretranslate"} {
		if t.providers[id] != nil {
			ids = append(ids, id)
		}
	}
	return ids
}

type translationCacheKey struct {
	owner   string
	id      int64
	content [sha256.Size]byte
}

func mailTranslationKey(owner string, id int64, provider, target, subject string, raw []byte) translationCacheKey {
	sum := sha256.New()
	sum.Write([]byte("v1\x00" + owner + "\x00" + provider + "\x00" + target + "\x00"))
	subjectHash := sha256.Sum256([]byte(subject))
	sum.Write(subjectHash[:])
	sum.Write(raw)
	return translationCacheKey{owner: owner, id: id, content: [sha256.Size]byte(sum.Sum(nil))}
}

func (a *app) cachedTranslation(ctx context.Context, key translationCacheKey) (translatedEmail, bool) {
	var value translatedEmail
	err := a.db.QueryRowContext(ctx, `SELECT c.subject,c.body FROM mail_translations c JOIN letters l ON l.id=c.letter_id
		WHERE c.letter_id=? AND c.cache_key=? AND c.expires_at>? AND l.owner=? AND l.folder<>'drafts'`,
		key.id, key.content[:], time.Now().Unix(), key.owner).Scan(&value.Subject, &value.Body)
	return value, err == nil
}

func (a *app) storeTranslation(ctx context.Context, key translationCacheKey, subject string, raw []byte, value translatedEmail) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize with mail deletion, draft changes and mailbox moves after the external call.
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM letters WHERE id=? AND owner=? AND folder<>'drafts' AND subject=? AND raw=?`,
		key.id, key.owner, subject, raw).Scan(&id); err != nil {
		return err
	}
	if len(value.Subject)+len(value.Body) > maxTranslationCacheEntry {
		return nil
	}
	query := `INSERT INTO mail_translations(letter_id,cache_key,subject,body,expires_at) VALUES(?,?,?,?,?)`
	if a.dbDriver == "mysql" {
		query += ` ON DUPLICATE KEY UPDATE subject=?,body=?,expires_at=?`
	} else {
		query += ` ON CONFLICT(letter_id,cache_key) DO UPDATE SET subject=?,body=?,expires_at=?`
	}
	expires := time.Now().Add(translationCacheTTL).Unix()
	if _, err := tx.ExecContext(ctx, query, id, key.content[:], value.Subject, value.Body, expires, value.Subject, value.Body, expires); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *app) pruneMailTranslations(ctx context.Context, now int64) error {
	for {
		result, err := a.db.ExecContext(ctx, `DELETE FROM mail_translations WHERE expires_at<=? AND (letter_id,cache_key) IN (
			SELECT letter_id,cache_key FROM (SELECT letter_id,cache_key FROM mail_translations
				WHERE expires_at<=? ORDER BY expires_at,letter_id,cache_key LIMIT 500) AS expired
		)`, now, now)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count < 500 {
			return err
		}
	}
}

func (a *app) maintainMailTranslations(ctx context.Context) {
	sweep, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := a.pruneMailTranslations(sweep, time.Now().Unix()); err != nil && ctx.Err() == nil {
		log.Print("Could not prune expired mail translations")
	}
}
