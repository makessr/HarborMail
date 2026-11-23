package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"

	"harbormail/internal/mailmime"
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
	view := mailmime.InspectHTML(raw, false)
	detail.HasHTML, detail.HasText = view.HasHTML, view.HasText
	return detail
}

func encodeMailDetail(raw []byte) []byte {
	value, _ := json.Marshal(inspectMailDetail(raw))
	return value
}

func ensureMailDetailColumn(ctx context.Context, db *sql.DB, driver string) error {
	if err := ensureColumn(ctx, db, driver, "letters", "detail_meta", "BLOB", "LONGBLOB"); err != nil {
		return err
	}
	return ensureColumn(ctx, db, driver, "letters", "delivery_job_id", "INTEGER", "BIGINT")
}
