package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"math"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
)

var migrationIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

var migrationTables = strings.Fields(`
	account_access account_password_overrides admin_account_actions admin_alias_actions admin_delivery_retries admin_delivery_resolutions admin_mfa_used
	app_passwords contacts custom_folders draft_creation_keys imap_deleted imap_uid_state imap_uids
	letters mail_label_links mail_labels mail_rules mail_snoozes mail_templates mail_threads
	mailbox_preferences mailbox_send_delay mailbox_storage_alerts mailbox_quota mailbox_quota_meta mailbox_retirements managed_accounts managed_aliases managed_domains
	outbox outbox_recipient_progress outbox_schedule saved_searches sessions signature_settings spam_feedback
	web_drafts web_letter_recipients web_outbox_envelopes web_session_details
`)

type migrationTable struct {
	name    string
	columns []string
	key     []string
}

func runSQLiteMigration(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("migrate-sqlite", flag.ContinueOnError)
	flags.SetOutput(output)
	filename := flags.String("sqlite", "", "path to the SQLite database or consistent backup")
	apply := flags.Bool("apply", false, "copy and verify data in the isolated MySQL database")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *filename == "" || flags.NArg() != 0 {
		return errors.New("usage: harbormail migrate-sqlite --sqlite PATH [--apply]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	abs, err := filepath.Abs(*filename)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("SQLite source must be an existing non-empty file")
	}
	path := filepath.ToSlash(abs)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	sourceURL := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	source, err := sql.Open("sqlite", sourceURL.String())
	if err != nil {
		return errors.New("cannot open SQLite source")
	}
	defer source.Close()
	read, err := source.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("open SQLite snapshot: %w", err)
	}
	defer read.Rollback()
	var integrity string
	if err := read.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return errors.New("SQLite quick check failed")
	}
	tables, err := readMigrationTables(ctx, read)
	if err != nil {
		return err
	}
	var inFlight int
	if err := read.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE state IN ('preparing','sending')`).Scan(&inFlight); err != nil {
		return fmt.Errorf("read source delivery queue: %w", err)
	}
	if inFlight != 0 {
		return errors.New("source has in-flight delivery tasks; stop the service and reconcile those tasks before migration")
	}
	dsn, err := fileSetting("HARBOR_MIGRATION_MYSQL_DSN")
	if err != nil {
		return err
	}
	if dsn == "" && !*apply {
		for _, table := range tables {
			var count int64
			if err := read.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+migrationQuote(table.name)).Scan(&count); err != nil {
				return err
			}
			fmt.Fprintf(output, "%s\t%d\n", table.name, count)
		}
		fmt.Fprintln(output, "SQLite source preflight passed")
		return nil
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil || config.DBName == "" {
		return errors.New("set HARBOR_MIGRATION_MYSQL_DSN or HARBOR_MIGRATION_MYSQL_DSN_FILE to an isolated MySQL database")
	}
	control, err := sql.Open("mysql", dsn)
	if err != nil {
		return errors.New("cannot configure MySQL migration target")
	}
	defer control.Close()
	connection, err := control.Conn(ctx)
	if err != nil {
		return errors.New("cannot connect to MySQL migration target")
	}
	defer connection.Close()
	// The connection owns this lock until it closes. The Harbor application must stay stopped.
	var locked sql.NullInt64
	if err := connection.QueryRowContext(ctx, `SELECT GET_LOCK(CONCAT('harbor-migrate-',LEFT(SHA2(DATABASE(),256),40)),0)`).Scan(&locked); err != nil || locked.Int64 != 1 {
		return errors.New("could not obtain the MySQL migration lock")
	}
	if err := requireEmptyMigrationTarget(ctx, connection); err != nil {
		return err
	}
	if !*apply {
		fmt.Fprintf(output, "SQLite source and empty MySQL target passed preflight (%d source tables)\n", len(tables))
		return nil
	}
	target, err := openDB("mysql", dsn)
	if err != nil {
		return fmt.Errorf("initialize target schema: %w", err)
	}
	defer target.Close()
	if err := preserveMigrationSequences(ctx, read, target); err != nil {
		return err
	}
	write, err := target.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer write.Rollback()
	for _, table := range tables {
		if table.name == "imap_uid_state" {
			if _, err := write.ExecContext(ctx, `DELETE FROM imap_uid_state WHERE id=1 AND next_uid=1`); err != nil {
				return err
			}
		}
		count, sourceDigest, err := copyMigrationTable(ctx, read, write, table)
		if err != nil {
			return fmt.Errorf("copy %s: %w", table.name, err)
		}
		targetCount, targetDigest, err := hashMigrationTable(ctx, write, table)
		if err != nil {
			return fmt.Errorf("verify %s: %w", table.name, err)
		}
		if targetCount != count || targetDigest != sourceDigest {
			return fmt.Errorf("verify %s: target rows differ from source", table.name)
		}
		fmt.Fprintf(output, "%s\t%d verified\n", table.name, count)
	}
	if err := write.Commit(); err != nil {
		return fmt.Errorf("MySQL commit did not return success; inspect the target before retrying: %w", err)
	}
	fmt.Fprintln(output, "Migration committed. Keep the SQLite source until cutover checks pass.")
	return nil
}

func preserveMigrationSequences(ctx context.Context, source *sql.Tx, target *sql.DB) error {
	var exists bool
	if err := source.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='sqlite_sequence')`).Scan(&exists); err != nil || !exists {
		return err
	}
	rows, err := source.QueryContext(ctx, `SELECT name,seq FROM sqlite_sequence WHERE seq>0`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var last int64
		if err := rows.Scan(&name, &last); err != nil {
			return err
		}
		if !slices.Contains(migrationTables, name) || last == math.MaxInt64 {
			return fmt.Errorf("unsupported SQLite sequence for %s", name)
		}
		var autoIncrement bool
		err := target.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns
			WHERE table_schema=DATABASE() AND table_name=? AND EXTRA LIKE '%auto_increment%')`, name).Scan(&autoIncrement)
		if err != nil || !autoIncrement {
			return fmt.Errorf("target table %s does not have an auto-increment counter", name)
		}
		if _, err := target.ExecContext(ctx, "ALTER TABLE "+migrationQuote(name)+" AUTO_INCREMENT="+strconv.FormatInt(last+1, 10)); err != nil {
			return fmt.Errorf("preserve sequence for %s: %w", name, err)
		}
	}
	return rows.Err()
}

func readMigrationTables(ctx context.Context, source *sql.Tx) ([]migrationTable, error) {
	rows, err := source.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT GLOB 'sqlite_*'
		AND name NOT IN ('mail_content_fts','mail_content_fts_data','mail_content_fts_idx','mail_content_fts_docsize',
			'mail_content_fts_config') ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []migrationTable
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if !slices.Contains(migrationTables, name) {
			return nil, fmt.Errorf("unsupported SQLite table %q", name)
		}
		tables = append(tables, migrationTable{name: name})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if !slices.ContainsFunc(tables, func(t migrationTable) bool { return t.name == "letters" }) {
		return nil, errors.New("source does not contain Harbor Mail messages")
	}
	for index := range tables {
		table := &tables[index]
		columns, err := source.QueryContext(ctx, `SELECT name,pk FROM pragma_table_info(?) ORDER BY cid`, table.name)
		if err != nil {
			return nil, err
		}
		keyParts := make(map[int]string)
		for columns.Next() {
			var name string
			var pk int
			if err := columns.Scan(&name, &pk); err != nil || !migrationIdentifier.MatchString(name) {
				columns.Close()
				return nil, fmt.Errorf("unsupported column in %s", table.name)
			}
			quoted := migrationQuote(name)
			table.columns = append(table.columns, quoted)
			if pk > 0 {
				keyParts[pk] = quoted
			}
		}
		err = columns.Err()
		columns.Close()
		if err != nil {
			return nil, err
		}
		switch table.name {
		case "mail_templates", "mailbox_preferences", "signature_settings":
			if len(keyParts) == 0 && slices.Contains(table.columns, migrationQuote("owner")) {
				keyParts[1] = migrationQuote("owner")
			}
		}
		if len(keyParts) == 0 {
			return nil, fmt.Errorf("cannot determine primary key for %s", table.name)
		}
		for i := 1; i <= len(keyParts); i++ {
			table.key = append(table.key, keyParts[i])
		}
	}
	return tables, nil
}

func requireEmptyMigrationTarget(ctx context.Context, target *sql.Conn) error {
	rows, err := target.QueryContext(ctx, `SELECT table_name,COALESCE(engine,'') FROM information_schema.tables WHERE table_schema=DATABASE()`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name, engine string
		if err := rows.Scan(&name, &engine); err != nil {
			return err
		}
		if (!slices.Contains(migrationTables, name) && name != "mailbox_revisions" && name != "login_rate_buckets") || !strings.EqualFold(engine, "InnoDB") {
			return fmt.Errorf("target has an unexpected or non-transactional table: %s", name)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, name := range names {
		predicate := ""
		if name == "imap_uid_state" {
			predicate = " WHERE id<>1 OR next_uid<>1"
		}
		var occupied bool
		if err := target.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+migrationQuote(name)+predicate+")").Scan(&occupied); err != nil {
			return err
		}
		if occupied {
			return fmt.Errorf("target is already populated: %s", name)
		}
	}
	return nil
}

func migrationQuote(name string) string { return "`" + name + "`" }

func copyMigrationTable(ctx context.Context, source, target *sql.Tx, table migrationTable) (int64, [32]byte, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(table.columns)), ",")
	statement, err := target.PrepareContext(ctx, "INSERT INTO "+migrationQuote(table.name)+" ("+
		strings.Join(table.columns, ",")+") VALUES ("+placeholders+")")
	if err != nil {
		return 0, [32]byte{}, err
	}
	defer statement.Close()
	return readMigrationRows(ctx, source, table, func(values []any) error {
		_, err := statement.ExecContext(ctx, values...)
		return err
	})
}

func hashMigrationTable(ctx context.Context, tx *sql.Tx, table migrationTable) (int64, [32]byte, error) {
	return readMigrationRows(ctx, tx, table, nil)
}

func readMigrationRows(ctx context.Context, tx *sql.Tx, table migrationTable, visit func([]any) error) (int64, [32]byte, error) {
	query := "SELECT " + strings.Join(table.columns, ",") + " FROM " + migrationQuote(table.name) + " ORDER BY " + strings.Join(table.key, ",")
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return 0, [32]byte{}, err
	}
	defer rows.Close()
	digest := sha256.New()
	values := make([]any, len(table.columns))
	pointers := make([]any, len(values))
	for index := range values {
		pointers[index] = &values[index]
	}
	var count int64
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return 0, [32]byte{}, err
		}
		for index, value := range values {
			// SQLite can return a typed nil byte slice for a zero-length BLOB.
			// MySQL sends that slice as SQL NULL unless it is made non-nil.
			if blob, ok := value.([]byte); ok && blob == nil {
				value = []byte{}
				values[index] = value
			}
			data, err := migrationValueBytes(value)
			if err != nil {
				return 0, [32]byte{}, err
			}
			writeMigrationValue(digest, data)
		}
		if visit != nil {
			if err := visit(values); err != nil {
				return 0, [32]byte{}, err
			}
		}
		count++
	}
	return count, [32]byte(digest.Sum(nil)), rows.Err()
}

func migrationValueBytes(value any) ([]byte, error) {
	switch value := value.(type) {
	case nil:
		return nil, nil
	case []byte:
		return value, nil
	case string:
		return []byte(value), nil
	case int64:
		return strconv.AppendInt(nil, value, 10), nil
	case uint64:
		return strconv.AppendUint(nil, value, 10), nil
	case bool:
		if value {
			return []byte("1"), nil
		}
		return []byte("0"), nil
	default:
		return nil, fmt.Errorf("unsupported migration field type %T", value)
	}
}

func writeMigrationValue(digest hash.Hash, data []byte) {
	var length [8]byte
	if data == nil {
		binary.BigEndian.PutUint64(length[:], ^uint64(0))
	} else {
		binary.BigEndian.PutUint64(length[:], uint64(len(data)))
	}
	_, _ = digest.Write(length[:])
	_, _ = digest.Write(data)
}
