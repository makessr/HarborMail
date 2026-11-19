package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"
)

func visibleCc(raw []byte) string {
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	list, _ := message.Header.AddressList("Cc")
	addresses := make([]string, 0, len(list))
	for _, recipient := range list {
		addresses = append(addresses, recipient.Address)
	}
	return strings.Join(addresses, ", ")
}

func (a *app) webDeliveryEnvelope(w http.ResponseWriter, ctx context.Context, db folderQueryer, owner string, fields webRecipients) (webEnvelope, bool) {
	envelope, err := a.resolveWebEnvelope(ctx, db, fields)
	if errors.Is(err, errMailboxRetired) {
		fail(w, http.StatusUnprocessableEntity, "至少一个收件邮箱已回收")
		return webEnvelope{}, false
	}
	if errors.Is(err, errInvalidWebRecipients) || (err == nil && len(envelope.Targets) == 0) {
		fail(w, 400, "请填写有效的收件、抄送或密送地址，每封邮件最多 20 个地址")
		return webEnvelope{}, false
	}
	if err != nil {
		fail(w, 503, "暂时无法确认收件地址的投递配置")
		return webEnvelope{}, false
	}
	if !a.hasRelayFor(owner) && len(envelope.externalAddresses()) != 0 {
		fail(w, 422, "尚未配置外部 SMTP 中继，可向本站账号发送邮件")
		return webEnvelope{}, false
	}
	return envelope, true
}

// The caller holds the mailbox write lock and owns rollback.
func (a *app) submitWebMail(w http.ResponseWriter, ctx context.Context, tx *sql.Tx, m letter, raw []byte, envelope webEnvelope, plan sendPlan) {
	if err := a.requireActiveAccount(ctx, tx, m.From); err != nil {
		if errors.Is(err, errAccountSuspended) {
			fail(w, http.StatusForbidden, "邮箱账号已停用")
		} else {
			letterError(w, err)
		}
		return
	}
	for _, target := range envelope.Targets {
		if err := a.checkDestination(ctx, tx, target.Address, target.Owner); err != nil {
			if errors.Is(err, errDestinationChanged) {
				fail(w, 409, "收件地址的投递配置已变更，请核对后重新发送")
			} else {
				letterError(w, err)
			}
			return
		}
	}
	if plan.deferred() {
		scheduled, err := scheduleDelivery(ctx, tx, m, raw, "", &envelope, plan)
		if !letterError(w, err) && !letterError(w, a.commitMailbox(tx, m.From)) {
			scheduled.reply(w)
		}
		return
	}
	if len(envelope.externalAddresses()) != 0 {
		id, err := enqueueLetter(tx, m, raw)
		if letterError(w, err) || letterError(w, saveWebEnvelope(ctx, tx, id, envelope)) ||
			letterError(w, a.commitMailbox(tx, m.From)) {
			return
		}
		reply(w, 202, map[string]int64{"queued": id})
		return
	}
	owners, err := a.deliverWebLocals(ctx, tx, m, raw, envelope.Targets, false)
	if !letterError(w, err) && !letterError(w, a.commitMailbox(tx, owners...)) {
		w.WriteHeader(201)
	}
}

func (a *app) deliverWebLocals(ctx context.Context, tx *sql.Tx, m letter, raw []byte, targets []webTarget, queued bool) ([]string, error) {
	var err error
	if queued {
		err = a.insertQueuedSent(ctx, tx, m, raw)
	} else {
		err = insert(tx, m.From, m, "sent", raw)
	}
	if err != nil {
		return nil, err
	}
	owners := []string{m.From}
	for _, target := range targets {
		if target.Owner == "" {
			continue
		}
		incoming := m
		incoming.To, incoming.recipients = target.Address, nil
		incoming.Cc, incoming.Bcc = "", ""
		if err := insertIncoming(ctx, tx, target.Owner, m.From, "inbox", incoming, raw); err != nil {
			return nil, err
		}
		owners = append(owners, target.Owner)
	}
	return owners, nil
}

// Commit local delivery with its progress marker before contacting the relay.
func (a *app) processWebLocals(job *outboxJob, hasExternal bool) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return false, err
	}
	for _, target := range job.envelope.Targets {
		if err := a.checkDestination(ctx, tx, target.Address, target.Owner); err != nil {
			return false, err
		}
	}
	id := job.message.ID
	guard := ` WHERE id=? AND claim=? AND state='preparing' AND lease>?`
	statement := `DELETE FROM outbox`
	if hasExternal {
		statement = `UPDATE outbox SET lease=lease`
	}
	result, err := tx.ExecContext(ctx, statement+guard, id, job.claim, time.Now().Unix())
	if err := requireAffectedRow(result, err); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if hasExternal {
		result, err := tx.ExecContext(ctx, `UPDATE web_outbox_envelopes SET local_done=TRUE
			WHERE outbox_id=? AND local_done=FALSE`, id)
		if err := requireAffectedRow(result, err); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `DELETE FROM outbox_schedule WHERE outbox_id=?`, id); err != nil {
			return false, err
		}
		if err := deleteWebEnvelope(ctx, tx, id); err != nil {
			return false, err
		}
	}
	owners, err := a.deliverWebLocals(ctx, tx, job.message, job.raw, job.envelope.Targets, true)
	if err != nil {
		return false, err
	}
	if err := a.commitMailbox(tx, owners...); err != nil {
		return false, err
	}
	job.localDone = true
	return hasExternal, nil
}
