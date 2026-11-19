package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/emersion/go-smtp"
)

type recipientProgress struct {
	Address  string `json:"address"`
	State    string `json:"state"`
	Attempts int    `json:"attempts"`
	Due      int64  `json:"due"`
	Code     int    `json:"code"`
}

type recipientQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readRecipientProgress(ctx context.Context, db recipientQueryer, sender string, jobID int64) ([]recipientProgress, error) {
	rows, err := db.QueryContext(ctx, `SELECT recipient,state,attempts,due,smtp_code
		FROM outbox_recipient_progress WHERE outbox_id=? AND sender=? ORDER BY recipient`, jobID, sender)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]recipientProgress, 0)
	for rows.Next() {
		var item recipientProgress
		if err := rows.Scan(&item.Address, &item.State, &item.Attempts, &item.Due, &item.Code); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (a *app) initializeRecipientProgress(ctx context.Context, job *outboxJob, addresses []string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx, `UPDATE outbox SET lease=lease WHERE id=? AND claim=? AND state='preparing' AND lease>?`,
		job.message.ID, job.claim, now)
	if err := requireAffectedRow(result, err); err != nil {
		return err
	}
	var initialized bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM outbox_recipient_progress WHERE outbox_id=?)`, job.message.ID).Scan(&initialized)
	if err != nil {
		return err
	}
	if !initialized {
		for _, address := range addresses {
			if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_recipient_progress
				(outbox_id,sender,recipient,state,attempts,due,smtp_code) VALUES(?,?,?,'queued',0,?,0)`,
				job.message.ID, job.message.From, address, now); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (a *app) processRecipientDelivery(ctx context.Context, job *outboxJob, addresses []string, connect relayConnector) error {
	if err := a.initializeRecipientProgress(ctx, job, addresses); err != nil {
		return err
	}
	job.tracked = true
	recipients, err := readRecipientProgress(ctx, a.db, job.message.From, job.message.ID)
	if err != nil {
		return err
	}
	for _, item := range recipients {
		now := time.Now()
		if item.State != "queued" || ctx.Err() != nil || (item.Due > now.Unix() && now.Before(job.expires())) {
			continue
		}
		if err := a.sendToOneRecipient(ctx, job, item, connect); err != nil {
			return err
		}
	}
	finish, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	recipients, err = readRecipientProgress(finish, a.db, job.message.From, job.message.ID)
	if err != nil {
		return err
	}
	state, reason, due := summarizeRecipients(recipients)
	return a.finishOutbox(job, state, reason, due)
}

func (a *app) sendToOneRecipient(ctx context.Context, job *outboxJob, item recipientProgress, connect relayConnector) error {
	if !time.Now().Before(job.expires()) {
		return a.recordRecipientAttempt(job, item, "preparing", "failed", 0, 0)
	}
	if err := a.checkDestination(ctx, a.db, item.Address, ""); errors.Is(err, errDestinationChanged) {
		return a.recordRecipientAttempt(job, item, "preparing", "failed", 550, 0)
	} else if err != nil {
		return a.recordRecipientResult(job, item, err, false)
	}
	client, closeClient, err := connect(ctx)
	if err != nil {
		return a.recordRecipientResult(job, item, err, false)
	}
	defer closeClient()
	if err := client.Mail(job.message.From, nil); err != nil {
		return a.recordRecipientResult(job, item, err, false)
	}
	if err := client.Rcpt(item.Address, nil); err != nil {
		return a.recordRecipientResult(job, item, err, false)
	}
	data, err := client.Data()
	if err != nil {
		return a.recordRecipientResult(job, item, err, false)
	}
	if _, err := io.Copy(data, bytes.NewReader(job.raw)); err != nil {
		return a.recordRecipientResult(job, item, err, false)
	}
	// Commit the only recipient at risk of replay before writing the DATA terminator.
	if err := a.recordRecipientAttempt(job, item, "preparing", "sending", 0, 0); err != nil {
		return err
	}
	return a.recordRecipientResult(job, item, data.Close(), true)
}

func (a *app) recordRecipientResult(job *outboxJob, item recipientProgress, cause error, submitted bool) error {
	phase := "preparing"
	if submitted {
		phase = "sending"
	}
	if cause == nil {
		return a.recordRecipientAttempt(job, item, phase, "accepted", 250, 0)
	}
	var smtpError *smtp.SMTPError
	code := 0
	if errors.As(cause, &smtpError) {
		code = smtpError.Code
		if code >= 500 && code < 600 && !errors.Is(cause, errRelayAuthentication) {
			return a.recordRecipientAttempt(job, item, phase, "failed", code, 0)
		}
	}
	if submitted && (code < 400 || code >= 500) {
		return a.recordRecipientAttempt(job, item, phase, "uncertain", code, 0)
	}
	delay := min(30*time.Minute<<min(item.Attempts, 4), 6*time.Hour)
	due := min(time.Now().Add(delay).Unix(), job.expires().Unix())
	return a.recordRecipientAttempt(job, item, phase, "queued", code, due)
}

func (a *app) recordRecipientAttempt(job *outboxJob, item recipientProgress, phase, state string, code int, due int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	nextPhase, prior, increment := "preparing", "queued", 1
	if phase == "sending" {
		prior, increment = "sending", 0
	}
	if state == "sending" {
		nextPhase = "sending"
	}
	now := time.Now()
	result, err := tx.ExecContext(ctx, `UPDATE outbox SET state=?,lease=? WHERE id=? AND claim=? AND state=? AND lease>?`,
		nextPhase, now.Add(outboxLease).Unix(), job.message.ID, job.claim, phase, now.Unix())
	if err := requireAffectedRow(result, err); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `UPDATE outbox_recipient_progress SET state=?,smtp_code=?,due=?,attempts=attempts+?
		WHERE outbox_id=? AND recipient=? AND state=?`, state, code, due, increment, job.message.ID, item.Address, prior)
	if err := requireAffectedRow(result, err); err != nil {
		return err
	}
	return a.commitMailbox(tx, job.message.From)
}

func summarizeRecipients(items []recipientProgress) (state, reason string, due int64) {
	var accepted, pending, failed, uncertain int
	for _, item := range items {
		switch item.State {
		case "accepted":
			accepted++
		case "queued":
			pending++
			if due == 0 || item.Due < due {
				due = item.Due
			}
		case "failed":
			failed++
		default:
			uncertain++
		}
	}
	reason = fmt.Sprintf("外部地址：%d 已接收，%d 等待重试，%d 失败，%d 待确认。", accepted, pending, failed, uncertain)
	switch {
	case pending > 0:
		return "queued", reason, due
	case uncertain > 0:
		return "uncertain", reason, 0
	case failed > 0:
		return "failed", reason, 0
	case accepted > 0:
		return "sent", "", 0
	default:
		return "failed", "未找到可投递的外部地址。", 0
	}
}

func (a *app) recoverOutboxJob(job *outboxJob) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if a.hardQuota {
		if err := lockIMAPUIDs(ctx, tx); err != nil {
			return err
		}
	}
	m := job.message
	// Hold the parent row before inspecting recipient results. Another worker
	// may have recorded SMTP's response since the recovery scan.
	result, err := tx.ExecContext(ctx, `UPDATE outbox SET lease=lease
		WHERE id=? AND state='sending' AND claim=? AND lease<=?`,
		m.ID, job.claim, time.Now().Unix())
	if err := requireAffectedRow(result, err); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `UPDATE outbox_recipient_progress SET state='uncertain',due=0
		WHERE outbox_id=? AND sender=? AND state='sending'`, m.ID, m.From)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	state, reason, due := "uncertain", "服务在等待中继确认时中断，请核对中继记录。", int64(0)
	if changed > 0 {
		recipients, err := readRecipientProgress(ctx, tx, m.From, m.ID)
		if err != nil {
			return err
		}
		state, reason, due = summarizeRecipients(recipients)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE outbox SET state=?,due=?,reason=?,claim='',lease=0 WHERE id=?`,
		state, due, reason, m.ID); err != nil {
		return err
	}
	if state == "uncertain" {
		if err := a.insertDeliveryNotice(tx, m, state, reason); err != nil {
			return err
		}
	}
	return a.commitMailbox(tx, m.From)
}
