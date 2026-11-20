package server

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-smtp"
)

const (
	outboxLease   = 5 * time.Minute
	outboxMaxAge  = 5 * 24 * time.Hour
	outboxTimeout = 75 * time.Second
)

type deliveryStatus struct {
	Recipients      []recipientProgress `json:"recipients,omitempty"`
	State           string              `json:"state"`
	Attempts        int                 `json:"attempts"`
	Due             int64               `json:"due"`
	SendAt          int64               `json:"sendAt,omitempty"`
	Reason          string              `json:"reason,omitempty"`
	UndoRemainingMs *int64              `json:"undoRemainingMs,omitempty"`
}

type outboxJob struct {
	tracked      bool
	message      letter
	raw          []byte
	claim        string
	attempts     int
	sendAt       int64
	localOwner   string
	envelope     *webEnvelope
	localDone    bool
	dkimPrepared bool
	dkimHeader   string
	dkimErr      error
	smtpCode     int
}

func (job *outboxJob) singleRecipient() string {
	if job.tracked {
		return ""
	}
	if job.envelope == nil {
		return job.message.To
	}
	if targets := job.envelope.externalAddresses(); len(targets) == 1 {
		return targets[0]
	}
	return ""
}

func (a *app) enqueue(m letter, raw []byte) (int64, error) {
	id, err := enqueueLetter(a.db, m, raw)
	if err == nil {
		a.events.notify(m.From)
		a.outboxWakeup.notify()
	}
	return id, err
}

type outboxWriter interface {
	Exec(string, ...any) (sql.Result, error)
}

func enqueueLetter(writer outboxWriter, m letter, raw []byte) (int64, error) {
	return insertOutbox(writer, m, raw, "queued", time.Now().Unix())
}

func insertOutbox(writer outboxWriter, m letter, raw []byte, state string, due int64) (int64, error) {
	result, err := writer.Exec(`INSERT INTO outbox
		(sender,recipient,subject,body,stamp,raw,state,due) VALUES(?,?,?,?,?,?,?,?)`,
		m.From, m.To, strings.ToValidUTF8(m.Subject, "\uFFFD"), strings.ToValidUTF8(m.Body, "\uFFFD"),
		m.Date, raw, state, due)
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, recordDeliveryEvent(writer, m.From, id, "", "enqueued", 0, due)
}

func (a *app) runOutbox(ctx context.Context, connectRelay func(context.Context, string) (*smtp.Client, func(), error)) {
	for ctx.Err() == nil {
		recoveryAt := time.Now().Unix()
		claimAt := recoveryAt
		retry := false
		if err := a.recoverOutbox(ctx); err != nil {
			retry = true
			if ctx.Err() == nil {
				log.Print("outbox recovery: ", err)
			}
		} else {
			for ctx.Err() == nil {
				claimAt = time.Now().Unix()
				job, err := a.claimOutbox()
				if err != nil {
					retry = true
					log.Print("outbox claim: ", err)
					break
				}
				if job == nil {
					break
				}
				sender := job.message.wireFrom()
				connect := relayForRecipient(func(ctx context.Context) (*smtp.Client, func(), error) {
					return connectRelay(ctx, sender)
				})
				if a.directMX != nil && !a.hasRelayFor(sender) {
					connect = a.directMX.connect
				}
				if err := a.processOutboxForRecipient(job, connect); err != nil {
					log.Print("outbox delivery state: ", err)
				}
			}
		}
		var next sql.NullInt64
		if !retry && ctx.Err() == nil {
			probe, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := a.db.QueryRowContext(probe, `SELECT MIN(deadline) FROM (
				SELECT MIN(due) AS deadline FROM outbox WHERE state IN ('queued','scheduled','undo') AND due>?
				UNION ALL
				SELECT MIN(lease) FROM outbox WHERE state IN ('preparing','sending') AND lease>?
			) AS deadlines`, claimAt, recoveryAt).Scan(&next)
			cancel()
			retry = err != nil
		}
		delay := a.taskPollInterval()
		if retry {
			delay = 3 * time.Second
		}
		a.outboxWakeup.wait(ctx, next, delay)
	}
}

func (a *app) recoverOutbox(ctx context.Context) error {
	for ctx.Err() == nil {
		var job outboxJob
		m := &job.message
		err := a.db.QueryRowContext(ctx, `SELECT id,sender,recipient,subject,claim FROM outbox
			WHERE state='sending' AND lease<=? ORDER BY lease,id LIMIT 1`, time.Now().Unix()).
			Scan(&m.ID, &m.From, &m.To, &m.Subject, &job.claim)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := a.recoverOutboxJob(&job); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (a *app) claimOutbox() (*outboxJob, error) {
	now := time.Now()
	var id int64
	var sender string
	args := []any{now.Unix(), now.Unix()}
	query := `SELECT id,sender FROM outbox WHERE ((state IN ('queued','scheduled','undo') AND due<=?)
		OR (state='preparing' AND lease<=?))`
	if a.directMX == nil && a.relay.Address == "" {
		// Mixed envelopes can still deliver their local targets without a relay.
		localTarget := `EXISTS (SELECT 1 FROM json_each(
			CASE WHEN json_valid(e.envelope) THEN e.envelope ELSE '{}' END, '$.Targets') AS target
			WHERE CASE WHEN target.type='object' THEN json_extract(target.value,'$.Owner')<>'' ELSE FALSE END)`
		if a.dbDriver == "mysql" {
			localTarget = `JSON_SEARCH(CASE WHEN JSON_VALID(e.envelope) THEN e.envelope ELSE '{}' END,
				'one','_%',NULL,'$.Targets[*].Owner') IS NOT NULL`
		}
		query += ` AND (EXISTS (SELECT 1 FROM outbox_schedule s WHERE s.outbox_id=outbox.id AND s.local_owner<>'')
			OR EXISTS (SELECT 1 FROM web_outbox_envelopes e WHERE e.outbox_id=outbox.id
				AND (e.has_external=FALSE OR (e.local_done=FALSE AND ` + localTarget + `)))
			OR (EXISTS (SELECT 1 FROM outbox_recipient_progress p WHERE p.outbox_id=outbox.id AND p.state IN ('accepted','failed','uncertain'))
				AND NOT EXISTS (SELECT 1 FROM outbox_recipient_progress p WHERE p.outbox_id=outbox.id AND p.state IN ('queued','sending')))
			OR (stamp<=? AND NOT EXISTS (SELECT 1 FROM outbox_schedule s WHERE s.outbox_id=outbox.id AND s.send_at>?))`
		// A sender without a relay may still have completed or expired deliveries to close.
		expiredBefore := now.Add(-outboxMaxAge).Unix()
		args = append(args, expiredBefore, expiredBefore)
		author := a.outboxAuthorSQL()
		for domain := range a.domainRelays {
			query += " OR " + author + " LIKE ? OR " + author + " LIKE ?"
			args = append(args, "%@"+domain, "%@"+domain+".")
		}
		query += ")"
	}
	err := a.db.QueryRow(query+` ORDER BY due,id LIMIT 1`, args...).Scan(&id, &sender)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	job := &outboxJob{claim: token()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE outbox SET state='preparing', claim=?, lease=?, attempts=attempts+1
		WHERE id=? AND sender=? AND ((state IN ('queued','scheduled','undo') AND due<=?) OR (state='preparing' AND lease<=?))`,
		job.claim, now.Add(outboxLease).Unix(), id, sender, now.Unix(), now.Unix())
	if err := requireAffectedRow(result, err); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := a.commitMailbox(tx, sender); err != nil {
		return nil, err
	}
	m := &job.message
	var envelopeData string
	err = a.db.QueryRow(`SELECT o.id,o.sender,o.recipient,o.subject,o.body,o.stamp,o.raw,o.attempts,
		coalesce(s.send_at,0),coalesce(s.local_owner,''),coalesce(e.envelope,''),coalesce(e.local_done,FALSE)
		FROM outbox o LEFT JOIN outbox_schedule s ON s.outbox_id=o.id
		LEFT JOIN web_outbox_envelopes e ON e.outbox_id=o.id
		WHERE o.id=? AND o.claim=?`, id, job.claim).Scan(
		&m.ID, &m.From, &m.To, &m.Subject, &m.Body, &m.Date, &job.raw, &job.attempts, &job.sendAt, &job.localOwner,
		&envelopeData, &job.localDone)
	if err != nil {
		return nil, err
	}
	job.envelope, err = parseWebEnvelope(envelopeData)
	if err != nil {
		return nil, err
	}
	if job.envelope != nil {
		m.recipients = &job.envelope.Recipients
		m.author, m.formatted = job.envelope.Author, job.envelope.Formatted
	}
	return job, nil
}

type relayConnector func(context.Context) (*smtp.Client, func(), error)

func (job *outboxJob) expires() time.Time {
	return time.Unix(max(job.message.Date, job.sendAt), 0).Add(outboxMaxAge)
}

func (a *app) processOutbox(job *outboxJob, connect relayConnector) error {
	return a.processOutboxForRecipient(job, relayForRecipient(connect))
}

func (a *app) processOutboxForRecipient(job *outboxJob, connect recipientConnector) error {
	ctx, cancel := context.WithTimeout(context.Background(), outboxTimeout)
	defer cancel()
	if err := a.requireSenderIdentity(ctx, a.db, job.message.From, job.message.author); errors.Is(err, errSendAs) {
		return a.finishOutbox(job, "failed", "此别名的发信权限已撤销。", 0)
	} else if err != nil {
		return err
	}
	multiExternal := job.envelope != nil && len(job.envelope.externalAddresses()) > 1
	if !multiExternal && !time.Now().Before(job.expires()) {
		return a.finishOutbox(job, "failed", "已超过 5 天投递期限。", 0)
	}
	if job.localOwner != "" {
		if err := a.deliverScheduledLocal(job); errors.Is(err, errDestinationChanged) {
			return a.finishOutbox(job, "failed", "原站内收件地址的投递配置已变更。", 0)
		} else if isQuotaExceeded(err) {
			return a.finishOutbox(job, "failed", "站内邮箱已达到硬配额。", 0)
		} else {
			return err
		}
	}
	recipients := []string{job.message.To}
	if job.envelope != nil {
		recipients = job.envelope.externalAddresses()
		if multiExternal && !time.Now().Before(job.expires()) {
			return a.processRecipientDelivery(ctx, job, recipients, connect)
		}
		if job.localDone && len(recipients) == 0 {
			return nil
		}
		hasLocal := !job.localDone && len(recipients) < len(job.envelope.Targets)
		if hasLocal {
			proceed, err := a.processWebLocals(job, len(recipients) != 0)
			if errors.Is(err, errSendAs) {
				return a.finishOutbox(job, "failed", "此别名的发信权限已撤销。", 0)
			}
			if errors.Is(err, errDestinationChanged) {
				return a.finishOutbox(job, "failed", "收件地址的投递配置已变更。", 0)
			}
			if isQuotaExceeded(err) {
				return a.finishOutbox(job, "failed", "站内邮箱已达到硬配额。", 0)
			}
			if err != nil || !proceed {
				return err
			}
		}
	}
	if multiExternal {
		return a.processRecipientDelivery(ctx, job, recipients, connect)
	}
	for _, recipient := range recipients {
		if err := a.checkDestination(ctx, a.db, recipient, ""); errors.Is(err, errDestinationChanged) {
			return a.finishOutbox(job, "failed", "收件地址的投递配置已变更。", 0)
		} else if err != nil {
			return err
		}
	}
	if len(recipients) == 0 {
		return a.finishOutbox(job, "failed", "邮件缺少可投递的收件人。", 0)
	}
	if err := a.prepareDKIM(ctx, job); err != nil {
		return a.resolveOutboxError(job, err, false)
	}
	client, closeClient, err := connect(ctx, recipients[0])
	if err != nil {
		return a.resolveOutboxError(job, err, false)
	}
	defer closeClient()
	if err := client.mail(job.message.wireFrom()); err != nil {
		return a.resolveOutboxError(job, err, false)
	}
	for _, recipient := range recipients {
		if err := client.rcpt(recipient); err != nil {
			return a.resolveOutboxError(job, err, false)
		}
	}
	data, err := client.data()
	if err != nil {
		return a.resolveOutboxError(job, err, false)
	}
	if err := job.writeExternalMessage(data); err != nil {
		return a.resolveOutboxError(job, err, false)
	}

	// Record the point of no safe replay before sending SMTP's DATA terminator.
	if err := a.markOutboxSending(ctx, job); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if err := data.Close(); err != nil {
		return a.resolveOutboxError(job, err, true)
	}
	job.smtpCode = 250
	return a.finishOutbox(job, "sent", "", 0)
}

func (a *app) markOutboxSending(ctx context.Context, job *outboxJob) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()
	result, err := tx.ExecContext(ctx, `UPDATE outbox SET state='sending', lease=?
		WHERE id=? AND claim=? AND state='preparing' AND lease>?`,
		now.Add(outboxLease).Unix(), job.message.ID, job.claim, now.Unix())
	if err := requireAffectedRow(result, err); err != nil {
		return err
	}
	if err := recordDeliveryEvent(tx, job.message.From, job.message.ID, job.singleRecipient(), "sending", 0, 0); err != nil {
		return err
	}
	return a.commitMailbox(tx, job.message.From)
}

func (a *app) resolveOutboxError(job *outboxJob, cause error, submitted bool) error {
	code := smtpResponseCode(cause)
	job.smtpCode = code
	reason := "无法连接投递服务器或投递暂时受阻。"
	if errors.Is(cause, errDKIMAuthor) {
		return a.finishOutbox(job, "failed", "DKIM 要求邮件 From 与发件地址一致。", 0)
	} else if errors.Is(cause, errDKIMSigning) {
		reason = "DKIM 签名暂不可用，请检查签名密钥和数据库。"
	} else if errors.Is(cause, errRelayAuthentication) {
		reason = "SMTP 中继认证失败，请检查中继凭据。"
	} else if code != 0 {
		if code >= 500 && code <= 599 {
			return a.finishOutbox(job, "failed", fmt.Sprintf("投递服务器拒绝了邮件（%d）。", code), 0)
		}
		if code >= 400 && code <= 499 {
			reason = fmt.Sprintf("投递服务器暂时拒绝了邮件（%d）。", code)
			submitted = false
		}
	}
	if submitted {
		return a.finishOutbox(job, "uncertain", "未收到投递服务器的最终确认，请先核对收件方或投递记录再决定是否重发。", 0)
	}
	delay := min(30*time.Minute<<min(job.attempts-1, 4), 6*time.Hour)
	due := min(time.Now().Add(delay).Unix(), job.expires().Unix())
	return a.finishOutbox(job, "queued", reason, due)
}

func (a *app) finishOutbox(job *outboxJob, state, reason string, due int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
	var result sql.Result
	m := job.message
	if state == "sent" {
		prior := "sending"
		if job.tracked {
			prior = "preparing"
		}
		result, err = tx.ExecContext(ctx, `DELETE FROM outbox WHERE id=? AND claim=? AND state=?`, m.ID, job.claim, prior)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE outbox SET state=?, reason=?, due=?, lease=0, claim=''
			WHERE id=? AND claim=? AND state IN ('preparing','sending')`, state, reason, due, m.ID, job.claim)
	}
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return err
	}
	if err := recordDeliveryEvent(tx, m.From, m.ID, job.singleRecipient(), state, job.smtpCode, due); err != nil {
		return err
	}
	switch state {
	case "sent":
		if job.sendAt != 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM outbox_schedule WHERE outbox_id=?`, m.ID); err != nil {
				return err
			}
		}
		if job.envelope != nil {
			if err := deleteWebEnvelope(ctx, tx, m.ID); err != nil {
				return err
			}
		}
		if !job.localDone {
			if err := a.insertQueuedSent(ctx, tx, m, job.raw); err != nil {
				return err
			}
		}
	case "failed", "uncertain":
		if err := a.insertDeliveryNotice(tx, m, state, reason); err != nil {
			return err
		}
	}
	return a.commitMailbox(tx, m.From)
}

func (a *app) insertDeliveryNotice(tx *sql.Tx, m letter, state, reason string) error {
	subject := "邮件投递失败"
	if state == "uncertain" {
		subject = "邮件投递结果待确认"
	}
	notice := letter{From: "postmaster@" + a.domain, To: m.From, Subject: subject, Date: time.Now().Unix(),
		Body: fmt.Sprintf("收件人：%s\n原邮件主题：%s\n\n%s\n\n可在 Harbor Mail 的「发件箱」查看原邮件。",
			m.To, m.Subject, reason)}
	raw, err := encodeLetter(notice, a.domain, nil)
	if err != nil {
		return err
	}
	err = insert(tx, notice.To, notice, "inbox", raw)
	if isQuotaExceeded(err) {
		return nil
	}
	return err
}

func dialRelay(ctx context.Context, addr string, implicit bool, cfg *tls.Config) (*smtp.Client, func(), error) {
	dialer := &net.Dialer{Timeout: 8 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	cleanup := func() {
		stop()
		_ = conn.Close()
	}
	var client *smtp.Client
	if implicit {
		encrypted := tls.Client(conn, cfg)
		if err := encrypted.HandshakeContext(ctx); err != nil {
			cleanup()
			return nil, nil, err
		}
		client = smtp.NewClient(encrypted)
	} else {
		client, err = smtp.NewClientStartTLS(conn, cfg)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	client.CommandTimeout = 15 * time.Second
	client.SubmissionTimeout = 45 * time.Second
	return client, cleanup, nil
}
