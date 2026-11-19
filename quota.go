package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/emersion/go-smtp"
)

const quotaExceededMarker = "harbor_mailbox_quota_exceeded"

func isQuotaExceeded(err error) bool {
	return err != nil && strings.Contains(err.Error(), quotaExceededMarker)
}

func smtpQuotaExceeded() error {
	return &smtp.SMTPError{Code: 552, EnhancedCode: smtp.EnhancedCode{5, 2, 2}, Message: "Mailbox storage quota exceeded"}
}

type quotaStatementRunner interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type quotaTable struct{ table, owner string }

var quotaTables = []quotaTable{{"letters", "owner"}, {"outbox", "sender"}}

func initHardQuota(db *sql.DB, driver string, enabled bool) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	var active bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mailbox_quota_meta WHERE id=1)`).Scan(&active); err != nil {
		return false, err
	}
	if !enabled && !active {
		return false, nil
	}
	if driver == "mysql" {
		conn, err := db.Conn(ctx)
		if err != nil {
			return false, err
		}
		defer conn.Close()
		var locked sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(CONCAT('harbor-quota-',LEFT(SHA2(DATABASE(),256),40)),60)`).Scan(&locked); err != nil {
			return false, err
		}
		if locked.Int64 != 1 {
			return false, errors.New("could not acquire the mailbox quota setup lock")
		}
		defer func() {
			unlock, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			_, _ = conn.ExecContext(unlock, `SELECT RELEASE_LOCK(CONCAT('harbor-quota-',LEFT(SHA2(DATABASE(),256),40)))`)
		}()
		var count int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.triggers
			WHERE trigger_schema=DATABASE() AND trigger_name IN
			('quota_letters_insert','quota_letters_update','quota_letters_delete','quota_outbox_insert','quota_outbox_update','quota_outbox_delete')`).Scan(&count); err != nil {
			return false, err
		}
		if count == 6 && active {
			return true, nil
		}
		for _, table := range quotaTables {
			if err := installQuotaTriggers(ctx, conn, table, true); err != nil {
				return false, err
			}
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return false, err
		}
		defer tx.Rollback()
		return true, rebuildQuota(ctx, tx, true)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return false, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name IN
		('quota_letters_insert','quota_letters_update','quota_letters_delete','quota_outbox_insert','quota_outbox_update','quota_outbox_delete')`).Scan(&count); err != nil {
		return false, err
	}
	if count == 6 && active {
		return true, tx.Commit()
	}
	for _, table := range quotaTables {
		if err := installQuotaTriggers(ctx, tx, table, false); err != nil {
			return false, err
		}
	}
	return true, rebuildQuota(ctx, tx, false)
}

func rebuildQuota(ctx context.Context, tx *sql.Tx, mysql bool) error {
	if _, err := tx.ExecContext(ctx, `UPDATE mailbox_quota SET used_bytes=0`); err != nil {
		return err
	}
	upsert := " ON CONFLICT(owner) DO UPDATE SET used_bytes=excluded.used_bytes"
	mark := " ON CONFLICT(id) DO NOTHING"
	if mysql {
		upsert = " ON DUPLICATE KEY UPDATE used_bytes=VALUES(used_bytes)"
		mark = " ON DUPLICATE KEY UPDATE id=id"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO mailbox_quota(owner,used_bytes,limit_mib)
		SELECT owner,SUM(size),0 FROM (
			SELECT owner,SUM(LENGTH(raw)) AS size FROM letters GROUP BY owner
			UNION ALL SELECT sender,SUM(LENGTH(raw)) FROM outbox GROUP BY sender
		) measured GROUP BY owner`+upsert)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_quota_meta(id) VALUES(1)`+mark); err != nil {
		return err
	}
	return tx.Commit()
}

func installQuotaTriggers(ctx context.Context, db quotaStatementRunner, table quotaTable, mysql bool) error {
	oldOwner, newOwner := "OLD."+table.owner, "NEW."+table.owner
	bump := func(owner, delta string) string {
		values := "INSERT INTO mailbox_quota(owner,used_bytes,limit_mib) VALUES(" + owner + "," + delta + ",0)"
		if mysql {
			return values + " ON DUPLICATE KEY UPDATE used_bytes=used_bytes+VALUES(used_bytes); "
		}
		return values + " ON CONFLICT(owner) DO UPDATE SET used_bytes=used_bytes+excluded.used_bytes; "
	}
	check := func(growth string) string {
		condition := growth + " AND EXISTS(SELECT 1 FROM mailbox_quota WHERE owner=" + newOwner +
			" AND limit_mib>0 AND used_bytes>limit_mib*1048576)"
		if mysql {
			return "IF " + condition + " THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='" + quotaExceededMarker + "'; END IF; "
		}
		return "SELECT CASE WHEN " + condition + " THEN RAISE(ABORT,'" + quotaExceededMarker + "') END; "
	}
	add := bump(newOwner, "LENGTH(NEW.raw)")
	remove := bump(oldOwner, "-LENGTH(OLD.raw)")
	changed := oldOwner + "<>" + newOwner + " OR LENGTH(OLD.raw)<>LENGTH(NEW.raw)"
	growth := "(" + oldOwner + "<>" + newOwner + " OR LENGTH(NEW.raw)>LENGTH(OLD.raw))"
	for _, event := range []string{"insert", "update", "delete"} {
		on := strings.ToUpper(event)
		when := ""
		var body string
		switch event {
		case "insert":
			body = add + check("TRUE")
		case "delete":
			body = remove
		case "update":
			body = remove + add + check(growth)
			if mysql {
				body = "IF " + changed + " THEN " + body + "END IF; "
			} else {
				on += " OF raw," + table.owner
				when = " WHEN " + changed
			}
		}
		query := fmt.Sprintf("CREATE TRIGGER IF NOT EXISTS quota_%s_%s AFTER %s ON %s FOR EACH ROW%s BEGIN %sEND",
			table.table, event, on, table.table, when, body)
		if _, err := db.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("create %s %s quota trigger: %w", table.table, event, err)
		}
	}
	return nil
}

func (a *app) adminSetHardQuota(w http.ResponseWriter, r *http.Request, actor string) {
	if !a.hardQuota {
		fail(w, http.StatusConflict, "服务器尚未启用硬配额")
		return
	}
	a.setStorageLimit(w, r, actor, true)
}

// The caller holds the mailbox write lock. A previously accepted queue job
// must be able to keep its Sent copy after the administrator lowers its limit.
func (a *app) insertQueuedSent(ctx context.Context, tx *sql.Tx, m letter, raw []byte) error {
	if !a.hardQuota {
		return insert(tx, m.From, m, "sent", raw)
	}
	var limit int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT limit_mib FROM mailbox_quota WHERE owner=?),0)`, m.From).Scan(&limit)
	if err != nil {
		return err
	}
	if limit == 0 {
		return insert(tx, m.From, m, "sent", raw)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mailbox_quota SET limit_mib=0 WHERE owner=?`, m.From); err != nil {
		return err
	}
	if err := insert(tx, m.From, m, "sent", raw); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE mailbox_quota SET limit_mib=? WHERE owner=?`, limit, m.From)
	return err
}
