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
	check := "SELECT COUNT(*) FROM pragma_table_info('letters') WHERE name='detail_meta'"
	column := "BLOB"
	if driver == "mysql" {
		check = "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='letters' AND column_name='detail_meta'"
		column = "LONGBLOB"
	}
	var count int
	if err := db.QueryRowContext(ctx, check).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return nil
	}
	_, err := db.ExecContext(ctx, "ALTER TABLE letters ADD COLUMN detail_meta "+column)
	var duplicate *mysql.MySQLError
	if errors.As(err, &duplicate) && duplicate.Number == 1060 {
		return nil // Another MySQL instance upgraded the shared schema first.
	}
	return err
}
