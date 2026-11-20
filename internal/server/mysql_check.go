package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/go-sql-driver/mysql"
)

func runMySQLCheck(out io.Writer) error {
	source, err := fileSetting("HARBOR_MYSQL_DSN")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return checkMySQL(ctx, out, source, os.Getenv("HARBOR_HARD_QUOTA") == "true")
}

func checkMySQL(ctx context.Context, out io.Writer, source string, hardQuota bool) error {
	cfg, err := harborMySQLConfig(source)
	if err != nil {
		return err
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return errors.New("could not configure MySQL connection; check the DSN and TLS settings")
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return mysqlCheckError("could not connect to MySQL; check the endpoint, credentials, database name and TLS settings", err)
	}
	defer conn.Close()
	var globalReadOnly, sessionReadOnly, logBin, trustCreators bool
	var serverPacket int
	err = conn.QueryRowContext(ctx, `SELECT @@GLOBAL.read_only OR @@GLOBAL.super_read_only,
		@@SESSION.transaction_read_only, @@SESSION.max_allowed_packet,
		@@GLOBAL.log_bin, @@GLOBAL.log_bin_trust_function_creators`).
		Scan(&globalReadOnly, &sessionReadOnly, &serverPacket, &logBin, &trustCreators)
	if err != nil {
		return mysqlCheckError("could not read MySQL server settings", err)
	}
	if globalReadOnly || sessionReadOnly {
		return errors.New("MySQL is read-only; use a writable endpoint and check the DSN session settings")
	}
	if serverPacket <= mailLimit || (cfg.MaxAllowedPacket > 0 && cfg.MaxAllowedPacket <= mailLimit) {
		return fmt.Errorf("the server max_allowed_packet and the driver limit must accommodate %d bytes of mail plus metadata (server limit %d bytes); check DSN maxAllowedPacket", mailLimit, serverPacket)
	}

	status := "Mail table: SELECT, INSERT, UPDATE and DELETE privileges verified on letters."
	for _, probe := range []struct{ action, query string }{
		{"SELECT", `EXPLAIN FORMAT=TRADITIONAL SELECT id, owner, sender, recipient, subject, body, stamp, folder, origin, previous, seen, star, raw FROM letters WHERE id=0`},
		{"INSERT", `EXPLAIN FORMAT=TRADITIONAL INSERT INTO letters (owner,sender,recipient,subject,body,stamp,folder,origin,raw) VALUES ('','','','','',0,'','',X'')`},
		{"UPDATE", `EXPLAIN FORMAT=TRADITIONAL UPDATE letters SET seen=TRUE WHERE id=0`},
		{"DELETE", `EXPLAIN FORMAT=TRADITIONAL DELETE FROM letters WHERE id=0`},
	} {
		rows, err := conn.QueryContext(ctx, probe.query)
		if err == nil {
			err = rows.Close()
		}
		if err == nil {
			continue
		}
		var mysqlErr *mysql.MySQLError
		if probe.action == "SELECT" && errors.As(err, &mysqlErr) && mysqlErr.Number == 1146 {
			status = "Mail table: letters is absent; privileges were not verified. Recheck after initialization."
			break
		}
		return mysqlCheckError("could not verify "+probe.action+" on letters; check privileges and schema", err)
	}
	if hardQuota && logBin && !trustCreators {
		if _, err := fmt.Fprintln(out, "Warning: MySQL binary logging restricts quota trigger creation. Confirm the server's trigger policy before enabling hard quotas."); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(out, "MySQL connection and writable settings verified. Server packet limit: %d bytes.\n%s\n", serverPacket, status)
	return err
}

func mysqlCheckError(message string, err error) error {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return fmt.Errorf("%s (MySQL error %d)", message, mysqlErr.Number)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", message, context.DeadlineExceeded)
	}
	return errors.New(message)
}
