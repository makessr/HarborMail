package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"

	"github.com/go-sql-driver/mysql"
)

type mailDetail struct {
	Recipients  messageHeaderRecipients
	Attachments []attachmentInfo
	HasHTML     bool
	HasText     bool
	Reply       string
}

func inspectMailDetail(raw []byte) mailDetail {
	detail := mailDetail{Recipients: visibleRecipients(raw), Reply: replyAddress(raw, "")}
	visitAttachments(raw, func(info attachmentInfo, _ io.Reader) bool {
		detail.Attachments = append(detail.Attachments, info)
		return true
	})
	view := inspectHTMLMail(raw, false)
	detail.HasHTML, detail.HasText = view.hasHTML, view.hasText
	return detail
}

func encodeMailDetail(raw []byte) []byte {
	value, _ := json.Marshal(inspectMailDetail(raw))
	return value
}

func ensureMailDetailColumn(ctx context.Context, db *sql.DB, driver string) error {
	if err := ensureLetterColumn(ctx, db, driver, "detail_meta", "BLOB", "LONGBLOB"); err != nil {
		return err
	}
	return ensureLetterColumn(ctx, db, driver, "delivery_job_id", "INTEGER", "BIGINT")
}

func ensureLetterColumn(ctx context.Context, db *sql.DB, driver, name, sqliteType, mysqlType string) error {
	check := "SELECT COUNT(*) FROM pragma_table_info('letters') WHERE name=?"
	column := sqliteType
	if driver == "mysql" {
		check = "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='letters' AND column_name=?"
		column = mysqlType
	}
	var count int
	if err := db.QueryRowContext(ctx, check, name).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, "ALTER TABLE letters ADD COLUMN "+name+" "+column)
	var duplicate *mysql.MySQLError
	if errors.As(err, &duplicate) && duplicate.Number == 1060 {
		return nil // Another MySQL instance upgraded the shared schema first.
	}
	return err
}
