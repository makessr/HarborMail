package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"
)

var sqliteSchema = []string{
	`PRAGMA busy_timeout=5000`,
	`PRAGMA journal_mode=WAL`,
	`CREATE TABLE IF NOT EXISTS letters (
		id INTEGER PRIMARY KEY, owner TEXT NOT NULL, sender TEXT NOT NULL,
		recipient TEXT NOT NULL, subject TEXT NOT NULL, body TEXT NOT NULL,
		stamp INTEGER NOT NULL, folder TEXT NOT NULL, origin TEXT NOT NULL, previous TEXT NOT NULL DEFAULT '',
		seen INTEGER NOT NULL DEFAULT 0, star INTEGER NOT NULL DEFAULT 0, raw BLOB NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS mailbox_order ON letters(owner, folder, id DESC)`,
	`CREATE TABLE IF NOT EXISTS spam_feedback (
		letter_id INTEGER PRIMARY KEY, owner TEXT NOT NULL, sample_class TEXT NOT NULL, revision INTEGER NOT NULL,
		state TEXT NOT NULL, due INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
		lease INTEGER NOT NULL DEFAULT 0, claim TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS spam_feedback_due ON spam_feedback(state,owner,due,lease)`,
	`CREATE TABLE IF NOT EXISTS spam_controller_pauses (
		controller TEXT NOT NULL PRIMARY KEY, pause_until INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS draft_creation_keys (
		owner TEXT NOT NULL, request_key TEXT NOT NULL, letter_id INTEGER NOT NULL, PRIMARY KEY(owner,request_key)
	)`,
	`CREATE TABLE IF NOT EXISTS mail_snoozes (
		letter_id INTEGER PRIMARY KEY, resume_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS mail_snoozes_due ON mail_snoozes(resume_at,letter_id)`,
	`CREATE TABLE IF NOT EXISTS sessions (
		digest BLOB PRIMARY KEY, owner TEXT NOT NULL, expires INTEGER NOT NULL, credential BLOB NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS admin_mfa_used (
		owner TEXT NOT NULL, enrollment BLOB NOT NULL, proof BLOB NOT NULL,
		expires INTEGER NOT NULL, PRIMARY KEY(owner,enrollment,proof)
	)`,
	`CREATE INDEX IF NOT EXISTS web_sessions_owner ON sessions(owner,expires)`,
	`CREATE INDEX IF NOT EXISTS web_sessions_expiry ON sessions(expires)`,
	`CREATE TABLE IF NOT EXISTS web_session_details (
		digest BLOB PRIMARY KEY, owner TEXT NOT NULL, device TEXT NOT NULL,
		created INTEGER NOT NULL, last_active INTEGER NOT NULL, expires INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS web_session_details_owner ON web_session_details(owner)`,
	`CREATE INDEX IF NOT EXISTS web_session_details_expiry ON web_session_details(expires)`,
	`CREATE TABLE IF NOT EXISTS outbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT, sender TEXT NOT NULL, recipient TEXT NOT NULL,
		subject TEXT NOT NULL, body TEXT NOT NULL, stamp INTEGER NOT NULL, raw BLOB NOT NULL,
		state TEXT NOT NULL, due INTEGER NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
		lease INTEGER NOT NULL DEFAULT 0, claim TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX IF NOT EXISTS outbox_sender ON outbox(sender,id)`,
	`CREATE INDEX IF NOT EXISTS outbox_ready ON outbox(state,due,lease)`,
	`CREATE TABLE IF NOT EXISTS outbox_recipient_progress (
		outbox_id INTEGER NOT NULL, sender TEXT NOT NULL, recipient TEXT NOT NULL,
		state TEXT NOT NULL, attempts INTEGER NOT NULL, due INTEGER NOT NULL, smtp_code INTEGER NOT NULL,
		PRIMARY KEY(outbox_id,recipient)
	)`,
	`CREATE INDEX IF NOT EXISTS outbox_recipient_sender ON outbox_recipient_progress(sender,outbox_id)`,
	`CREATE TABLE IF NOT EXISTS admin_delivery_retries (
		id INTEGER PRIMARY KEY AUTOINCREMENT, job_id INTEGER NOT NULL,
		actor TEXT NOT NULL, sender TEXT NOT NULL, previous_due INTEGER NOT NULL, stamp INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS admin_delivery_resolutions (
		id INTEGER PRIMARY KEY AUTOINCREMENT, job_id INTEGER NOT NULL,
		actor TEXT NOT NULL, sender TEXT NOT NULL, action TEXT NOT NULL, stamp INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS admin_account_actions (
		id INTEGER PRIMARY KEY AUTOINCREMENT, actor TEXT NOT NULL, target TEXT NOT NULL,
		action VARCHAR(24) NOT NULL, affected INTEGER NOT NULL, stamp INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS admin_account_actions_target ON admin_account_actions(target,id)`,
	`CREATE TABLE IF NOT EXISTS account_access (owner TEXT PRIMARY KEY, suspended BOOLEAN NOT NULL DEFAULT FALSE)`,
	`CREATE TABLE IF NOT EXISTS managed_accounts (
		email TEXT NOT NULL PRIMARY KEY, password_hash BLOB NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS mailbox_retirements (
		owner TEXT NOT NULL PRIMARY KEY, retired_at INTEGER NOT NULL, previous_suspended BOOLEAN NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS managed_domains (name TEXT NOT NULL PRIMARY KEY)`,
	`CREATE TABLE IF NOT EXISTS mailbox_storage_alerts (owner TEXT NOT NULL PRIMARY KEY, threshold_mib INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS mailbox_quota (owner TEXT NOT NULL PRIMARY KEY, used_bytes INTEGER NOT NULL, limit_mib INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS mailbox_quota_meta (id INTEGER NOT NULL PRIMARY KEY)`,
	`CREATE TABLE IF NOT EXISTS account_password_overrides (
		owner TEXT NOT NULL PRIMARY KEY, base_digest BLOB NOT NULL, password_hash BLOB NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS managed_aliases (alias TEXT NOT NULL PRIMARY KEY, owner TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS managed_aliases_owner ON managed_aliases(owner)`,
	`CREATE TABLE IF NOT EXISTS admin_alias_actions (
		id INTEGER PRIMARY KEY AUTOINCREMENT, actor TEXT NOT NULL, target TEXT NOT NULL,
		alias TEXT NOT NULL, action VARCHAR(24) NOT NULL, stamp INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS outbox_schedule (
		outbox_id INTEGER PRIMARY KEY, send_at INTEGER NOT NULL, local_owner TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS mail_rules (
		id INTEGER PRIMARY KEY AUTOINCREMENT, owner TEXT NOT NULL, rule_order INTEGER NOT NULL,
		enabled BOOLEAN NOT NULL, match_field TEXT NOT NULL, match_value TEXT NOT NULL,
		rule_action TEXT NOT NULL, destination TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS mail_rules_owner ON mail_rules(owner,rule_order,id)`,
	`CREATE TABLE IF NOT EXISTS mail_threads (
		letter_id INTEGER PRIMARY KEY, owner TEXT NOT NULL,
		message_key BLOB, root_key BLOB, parent_key BLOB
	)`,
	`CREATE INDEX IF NOT EXISTS thread_message ON mail_threads(owner,message_key)`,
	`CREATE INDEX IF NOT EXISTS thread_root ON mail_threads(owner,root_key)`,
	`CREATE INDEX IF NOT EXISTS thread_parent ON mail_threads(owner,parent_key)`,
	`CREATE TABLE IF NOT EXISTS contacts (
		id INTEGER PRIMARY KEY AUTOINCREMENT, owner TEXT NOT NULL,
		email TEXT COLLATE NOCASE NOT NULL, name TEXT NOT NULL, note TEXT NOT NULL,
		search_key TEXT NOT NULL, UNIQUE(owner,email)
	)`,
	`CREATE INDEX IF NOT EXISTS contacts_by_owner ON contacts(owner,id DESC)`,
	`CREATE TABLE IF NOT EXISTS app_passwords (
		id INTEGER PRIMARY KEY AUTOINCREMENT, owner TEXT NOT NULL,
		digest BLOB NOT NULL UNIQUE, credential BLOB NOT NULL, label TEXT NOT NULL,
		protocols INTEGER NOT NULL, created INTEGER NOT NULL, expires INTEGER NOT NULL, last_used INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS app_password_owner ON app_passwords(owner,id DESC)`,
	`CREATE TABLE IF NOT EXISTS mail_labels (
		id INTEGER PRIMARY KEY AUTOINCREMENT, owner TEXT NOT NULL,
		name TEXT NOT NULL, name_key TEXT NOT NULL, UNIQUE(owner,name_key)
	)`,
	`CREATE TABLE IF NOT EXISTS mail_label_links (
		letter_id INTEGER NOT NULL, label_id INTEGER NOT NULL, PRIMARY KEY(letter_id,label_id)
	)`,
	`CREATE INDEX IF NOT EXISTS mail_label_members ON mail_label_links(label_id,letter_id)`,
	`CREATE TABLE IF NOT EXISTS signature_settings (
		owner TEXT NOT NULL UNIQUE, revision INTEGER NOT NULL, settings TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS mail_templates (
		owner TEXT NOT NULL UNIQUE, revision INTEGER NOT NULL, templates TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS mailbox_preferences (
		owner TEXT NOT NULL UNIQUE, revision INTEGER NOT NULL,
		theme TEXT NOT NULL, density TEXT NOT NULL, read_mode TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS mailbox_send_delay (
		owner TEXT NOT NULL PRIMARY KEY, delay_seconds INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS saved_searches (
		id INTEGER PRIMARY KEY AUTOINCREMENT, owner TEXT NOT NULL, revision INTEGER NOT NULL,
		name TEXT NOT NULL, name_key TEXT NOT NULL, folder TEXT NOT NULL, label_id INTEGER NOT NULL,
		keyword TEXT NOT NULL, filters TEXT NOT NULL, UNIQUE(owner,name_key)
	)`,
}

var mysqlSchema = []string{
	`CREATE TABLE IF NOT EXISTS login_rate_buckets (
		bucket BINARY(32) NOT NULL PRIMARY KEY,
		attempts INT NOT NULL,
		expires BIGINT NOT NULL,
		KEY login_rate_expiry (expires)
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS mailbox_revisions (
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
		revision BIGINT NOT NULL
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS letters (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
		sender MEDIUMTEXT NOT NULL,
		recipient VARCHAR(254) NOT NULL,
		subject MEDIUMTEXT NOT NULL,
		body MEDIUMTEXT NOT NULL,
		stamp BIGINT NOT NULL,
		folder VARCHAR(16) NOT NULL,
		origin VARCHAR(16) NOT NULL,
		previous VARCHAR(16) NOT NULL DEFAULT '',
		seen BOOLEAN NOT NULL DEFAULT FALSE,
		star BOOLEAN NOT NULL DEFAULT FALSE,
		raw LONGBLOB NOT NULL,
		KEY mailbox_order (owner, folder, id)
	) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS spam_feedback (
		letter_id BIGINT NOT NULL PRIMARY KEY,
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
		sample_class VARCHAR(4) NOT NULL, revision BIGINT NOT NULL, state VARCHAR(8) NOT NULL,
		due BIGINT NOT NULL, attempts INT NOT NULL DEFAULT 0,
		lease BIGINT NOT NULL DEFAULT 0, claim CHAR(64) NOT NULL DEFAULT '',
		KEY spam_feedback_due (state,owner,due,lease)
	) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS spam_controller_pauses (
		controller CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY, pause_until BIGINT NOT NULL
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS draft_creation_keys (
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
		request_key CHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
		letter_id BIGINT NOT NULL, PRIMARY KEY(owner,request_key)
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS mail_snoozes (
		letter_id BIGINT NOT NULL PRIMARY KEY, resume_at BIGINT NOT NULL,
		KEY mail_snoozes_due (resume_at,letter_id)
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS sessions (
		digest BINARY(32) NOT NULL PRIMARY KEY,
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
		expires BIGINT NOT NULL,
		credential BINARY(32) NOT NULL,
		KEY session_expiry (expires), KEY web_sessions_owner (owner,expires)
	) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS web_session_details (
		digest BINARY(32) PRIMARY KEY, owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
		device VARCHAR(80) NOT NULL, created BIGINT NOT NULL, last_active BIGINT NOT NULL, expires BIGINT NOT NULL,
		KEY web_session_details_owner (owner), KEY web_session_details_expiry (expires)
	) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS outbox (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		sender VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
		recipient VARCHAR(254) NOT NULL, subject MEDIUMTEXT NOT NULL, body MEDIUMTEXT NOT NULL,
		stamp BIGINT NOT NULL, raw LONGBLOB NOT NULL, state VARCHAR(16) NOT NULL,
		due BIGINT NOT NULL, attempts INT NOT NULL DEFAULT 0, lease BIGINT NOT NULL DEFAULT 0,
		claim CHAR(64) NOT NULL DEFAULT '', reason VARCHAR(160) NOT NULL DEFAULT '',
		KEY outbox_sender (sender,id), KEY outbox_ready (state,due,lease)
	) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS outbox_recipient_progress (
		outbox_id BIGINT NOT NULL, sender VARCHAR(254) NOT NULL, recipient VARCHAR(254) NOT NULL,
		state VARCHAR(16) NOT NULL, attempts INT NOT NULL, due BIGINT NOT NULL, smtp_code INT NOT NULL,
		PRIMARY KEY(outbox_id,recipient), KEY outbox_recipient_sender(sender,outbox_id)
	) ENGINE=InnoDB DEFAULT CHARACTER SET ascii COLLATE ascii_bin`,
	`CREATE TABLE IF NOT EXISTS admin_delivery_retries (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, job_id BIGINT NOT NULL,
		actor VARCHAR(254) NOT NULL, sender VARCHAR(254) NOT NULL, previous_due BIGINT NOT NULL, stamp BIGINT NOT NULL
	) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS admin_delivery_resolutions (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, job_id BIGINT NOT NULL,
		actor VARCHAR(254) NOT NULL, sender VARCHAR(254) NOT NULL, action VARCHAR(24) NOT NULL, stamp BIGINT NOT NULL
	) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS admin_account_actions (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, actor VARCHAR(254) NOT NULL, target VARCHAR(254) NOT NULL,
		action VARCHAR(24) NOT NULL, affected BIGINT NOT NULL, stamp BIGINT NOT NULL,
		KEY admin_account_actions_target (target,id)
	) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS account_access (
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY, suspended BOOLEAN NOT NULL DEFAULT FALSE
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS admin_mfa_used (
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
		enrollment BINARY(32) NOT NULL, proof BINARY(32) NOT NULL,
		expires BIGINT NOT NULL, PRIMARY KEY(owner,enrollment,proof)
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS managed_accounts (
		email VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY, password_hash VARBINARY(72) NOT NULL
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS mailbox_retirements (
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
		retired_at BIGINT NOT NULL, previous_suspended BOOLEAN NOT NULL
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS account_password_overrides (
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
		base_digest BINARY(32) NOT NULL, password_hash VARBINARY(72) NOT NULL
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS managed_aliases (
		alias VARCHAR(254) NOT NULL PRIMARY KEY, owner VARCHAR(254) NOT NULL, KEY managed_aliases_owner (owner)
	)
		ENGINE=InnoDB DEFAULT CHARACTER SET ascii COLLATE ascii_bin`,
	`CREATE TABLE IF NOT EXISTS admin_alias_actions (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, actor VARCHAR(254) NOT NULL, target VARCHAR(254) NOT NULL,
		alias VARCHAR(254) NOT NULL, action VARCHAR(24) NOT NULL, stamp BIGINT NOT NULL,
		KEY admin_alias_action_time (stamp,id)
	) ENGINE=InnoDB DEFAULT CHARACTER SET ascii COLLATE ascii_bin`,
	`CREATE TABLE IF NOT EXISTS managed_domains (name VARCHAR(253) NOT NULL PRIMARY KEY)
		ENGINE=InnoDB DEFAULT CHARACTER SET ascii COLLATE ascii_bin`,
	`CREATE TABLE IF NOT EXISTS mailbox_storage_alerts (
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY, threshold_mib BIGINT NOT NULL
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS mailbox_quota (
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY, used_bytes BIGINT NOT NULL, limit_mib BIGINT NOT NULL
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS mailbox_quota_meta (id INTEGER NOT NULL PRIMARY KEY) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS outbox_schedule (
		outbox_id BIGINT PRIMARY KEY, send_at BIGINT NOT NULL,
		local_owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS mail_rules (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
		rule_order INTEGER NOT NULL, enabled BOOLEAN NOT NULL,
		match_field VARCHAR(16) NOT NULL, match_value VARCHAR(254) NOT NULL,
		rule_action VARCHAR(16) NOT NULL, destination VARCHAR(16) NOT NULL,
		KEY mail_rules_owner (owner,rule_order,id)
	) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS mail_threads (
		letter_id BIGINT NOT NULL PRIMARY KEY,
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
		message_key BINARY(32), root_key BINARY(32), parent_key BINARY(32),
		KEY thread_message (owner,message_key), KEY thread_root (owner,root_key), KEY thread_parent (owner,parent_key)
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS contacts (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
		email VARCHAR(254) CHARACTER SET ascii COLLATE ascii_general_ci NOT NULL,
		name VARCHAR(100) NOT NULL, note VARCHAR(500) NOT NULL, search_key VARCHAR(355) NOT NULL,
		UNIQUE KEY contact_email (owner,email), KEY contacts_by_owner (owner,id)
	) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS app_passwords (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
		digest BINARY(32) NOT NULL UNIQUE, credential BINARY(32) NOT NULL,
		label VARCHAR(80) NOT NULL, protocols INTEGER NOT NULL,
		created BIGINT NOT NULL, expires BIGINT NOT NULL, last_used BIGINT NOT NULL DEFAULT 0,
		KEY app_password_owner (owner,id)
	) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS mail_labels (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
		name VARCHAR(40) NOT NULL, name_key VARCHAR(40) NOT NULL,
		UNIQUE KEY mail_label_name (owner,name_key)
	) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS mail_label_links (
		letter_id BIGINT NOT NULL, label_id BIGINT NOT NULL, PRIMARY KEY(letter_id,label_id),
		KEY mail_label_members (label_id,letter_id)
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS signature_settings (
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
		revision BIGINT NOT NULL, settings MEDIUMTEXT NOT NULL
	) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS mail_templates (
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
		revision BIGINT NOT NULL, templates MEDIUMTEXT NOT NULL
	) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS mailbox_preferences (
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
		revision BIGINT NOT NULL, theme VARCHAR(8) NOT NULL, density VARCHAR(12) NOT NULL, read_mode VARCHAR(8) NOT NULL
	) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS mailbox_send_delay (
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
		delay_seconds INT NOT NULL
	) ENGINE=InnoDB`,
	`CREATE TABLE IF NOT EXISTS saved_searches (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		owner VARCHAR(254) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
		revision BIGINT NOT NULL, name VARCHAR(40) NOT NULL, name_key VARCHAR(40) NOT NULL,
		folder VARCHAR(16) NOT NULL, label_id BIGINT NOT NULL, keyword TEXT NOT NULL, filters TEXT NOT NULL,
		UNIQUE KEY saved_search_name (owner,name_key)
	) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
}

func checkDataDir(dir string) error {
	probe, err := os.CreateTemp(dir, ".harbor-write-check-*")
	if err != nil {
		return err
	}
	_, writeErr := probe.WriteString("ok\n")
	return errors.Join(writeErr, probe.Close(), os.Remove(probe.Name()))
}

func openDB(driver, source string) (*sql.DB, error) {
	var db *sql.DB
	var schema []string
	switch driver {
	case "sqlite":
		if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
			return nil, err
		}
		var err error
		db, err = sql.Open(driver, source)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(1)
		schema = sqliteSchema
	case "mysql":
		cfg, err := harborMySQLConfig(source)
		if err != nil {
			return nil, err
		}
		connector, err := mysql.NewConnector(cfg)
		if err != nil {
			return nil, fmt.Errorf("configure MySQL connection: %w", err)
		}
		db = sql.OpenDB(connector)
		db.SetMaxOpenConns(10)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(3 * time.Minute)
		schema = mysqlSchema
	default:
		return nil, fmt.Errorf("unsupported HARBOR_DB_DRIVER %q (use sqlite or mysql)", driver)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect to %s database: %w", driver, err)
	}
	for _, statement := range schema {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("initialize %s database: %w", driver, err)
		}
	}
	if err := initIMAPSchema(ctx, db, driver); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize %s IMAP metadata: %w", driver, err)
	}
	if err := initWebRecipientSchema(ctx, db, driver); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize %s web recipient metadata: %w", driver, err)
	}
	if err := renameLegacyJunkFolders(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("reserve the Junk folder name: %w", err)
	}
	return db, nil
}

func harborMySQLConfig(source string) (*mysql.Config, error) {
	cfg, err := mysql.ParseDSN(source)
	if err != nil || cfg.DBName == "" {
		return nil, errors.New("HARBOR_MYSQL_DSN must contain a valid MySQL DSN with a database name")
	}
	cfg.ClientFoundRows = true
	if err := cfg.Apply(mysql.Charset("utf8mb4", "utf8mb4_bin")); err != nil {
		return nil, fmt.Errorf("configure MySQL character set: %w", err)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 20 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 20 * time.Second
	}
	return cfg, nil
}

func initIMAPSchema(ctx context.Context, db *sql.DB, driver string) error {
	engine := ""
	upsert := " ON CONFLICT(id) DO NOTHING"
	if driver == "mysql" {
		engine = " ENGINE=InnoDB"
		upsert = " ON DUPLICATE KEY UPDATE id=id"
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS imap_uid_state (
		id INTEGER NOT NULL PRIMARY KEY, validity BIGINT NOT NULL, next_uid BIGINT NOT NULL
	)`+engine); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS imap_uids (
		letter_id BIGINT NOT NULL PRIMARY KEY, uid BIGINT NOT NULL UNIQUE, folder VARCHAR(16) NOT NULL
	)`+engine); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS imap_deleted (
		letter_id BIGINT NOT NULL PRIMARY KEY, uid BIGINT NOT NULL
	)`+engine); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS web_drafts (
		letter_id BIGINT NOT NULL PRIMARY KEY, revision BIGINT NOT NULL
	)`+engine); err != nil {
		return err
	}
	folderOwner, folderLookup, folderCharset := "VARCHAR(254)", "VARCHAR(128)", ""
	if driver == "mysql" {
		folderOwner += " CHARACTER SET ascii COLLATE ascii_bin"
		folderLookup += " CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"
		folderCharset = " DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS custom_folders (
		folder VARCHAR(16) NOT NULL PRIMARY KEY, owner `+folderOwner+` NOT NULL,
		label VARCHAR(128) NOT NULL, lookup `+folderLookup+` NOT NULL,
		subscribed BOOLEAN NOT NULL DEFAULT TRUE, UNIQUE(owner,lookup)
	)`+engine+folderCharset); err != nil {
		return err
	}
	validity := uint64(rand.Uint32()) + 1
	if validity > 0xffffffff {
		validity = 1
	}
	_, err := db.ExecContext(ctx, `INSERT INTO imap_uid_state VALUES(1,?,1)`+upsert, validity)
	return err
}

func clearIMAPMembership(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM imap_deleted WHERE letter_id=?`, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM imap_uids WHERE letter_id=?`, id)
	return err
}

func lockIMAPUIDs(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE imap_uid_state SET next_uid=next_uid WHERE id=1`)
	return err
}

func requireAffectedRow(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return sql.ErrNoRows
	}
	return err
}

func invalidateMovedUID(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM imap_uids WHERE letter_id=?
		AND folder<>(SELECT folder FROM letters WHERE id=?)`, id, id)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM imap_deleted WHERE letter_id=?
		AND NOT EXISTS (SELECT 1 FROM imap_uids WHERE letter_id=?)`, id, id)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE web_drafts SET revision=revision+1 WHERE letter_id=?`, id)
	return err
}
