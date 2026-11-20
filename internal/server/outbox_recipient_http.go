package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"
)

func (a *app) outboxRecipientProgress(w http.ResponseWriter, r *http.Request, owner string) {
	items, err := readRecipientProgress(r.Context(), a.db, owner, letterID(r))
	if letterError(w, err) {
		return
	}
	if len(items) == 0 {
		fail(w, http.StatusNotFound, "没有该邮件的按地址投递记录")
		return
	}
	reply(w, http.StatusOK, items)
}

func (a *app) resolveOutboxRecipient(w http.ResponseWriter, r *http.Request, owner string) {
	var input struct{ Address, Outcome string }
	if !decode(w, r, &input) {
		return
	}
	recipient, err := address(input.Address)
	if err != nil || recipient != input.Address || (input.Outcome != "accepted" && input.Outcome != "failed") {
		fail(w, http.StatusBadRequest, "请选择已核实的单地址投递结果")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if letterError(w, err) {
		return
	}
	defer tx.Rollback()
	if letterError(w, lockIMAPUIDs(ctx, tx)) ||
		browserSessionError(w, a.lockBrowserSession(ctx, tx, owner, browserSessionDigest(r))) {
		return
	}
	id := letterID(r)
	result, err := tx.ExecContext(ctx, `UPDATE outbox SET due=due WHERE id=? AND sender=? AND state='uncertain' AND claim='' AND lease=0`,
		id, owner)
	if err := requireAffectedRow(result, err); errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusConflict, "邮件状态已变化，请刷新发件箱")
		return
	} else if letterError(w, err) {
		return
	}
	result, err = tx.ExecContext(ctx, `UPDATE outbox_recipient_progress SET state=?,due=0
		WHERE outbox_id=? AND sender=? AND recipient=? AND state='uncertain'`, input.Outcome, id, owner, recipient)
	if err := requireAffectedRow(result, err); errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusConflict, "收件地址状态已变化，请重新打开该邮件")
		return
	} else if letterError(w, err) {
		return
	}
	items, err := readRecipientProgress(ctx, tx, owner, id)
	if letterError(w, err) {
		return
	}
	state, reason, due := summarizeRecipients(items)
	if state == "sent" {
		err = a.confirmUncertainRelayDelivery(ctx, tx, id)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE outbox SET state=?,reason=?,due=? WHERE id=? AND sender=?`,
			state, reason, due, id, owner)
	}
	if letterError(w, err) {
		return
	}
	action := "recipient_rejected"
	if input.Outcome == "accepted" {
		action = "recipient_confirmed"
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_delivery_resolutions(job_id,actor,sender,action,stamp)
		VALUES(?,?,?,?,?)`, id, owner, owner, action, time.Now().Unix())
	if !letterError(w, err) && !letterError(w, a.commitOutbox(tx, owner)) {
		w.WriteHeader(http.StatusNoContent)
	}
}
