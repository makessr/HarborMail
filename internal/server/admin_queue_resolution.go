package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"
)

func (a *app) adminResolveDelivery(w http.ResponseWriter, r *http.Request, actor string) {
	id := letterID(r)
	var input struct {
		Attempts int
		Outcome  string
		Password string
	}
	if !decode(w, r, &input) {
		return
	}
	if id < 1 || input.Attempts < 1 || (input.Outcome != "accepted" && input.Outcome != "rejected") {
		fail(w, http.StatusBadRequest, "请选择中继已核实的投递结果")
		return
	}
	if _, err := a.authenticate(actor, input.Password); err != nil {
		if errors.Is(err, errLoginBusy) {
			fail(w, http.StatusTooManyRequests, "验证请求过多，请稍后重试")
		} else {
			fail(w, http.StatusForbidden, "管理员主邮箱密码不正确")
		}
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
		browserSessionError(w, a.lockBrowserSession(ctx, tx, actor, browserSessionDigest(r))) {
		return
	}
	result, err := tx.ExecContext(ctx, `UPDATE outbox SET state='failed',reason=?
		WHERE id=? AND state='uncertain' AND attempts=? AND claim='' AND lease=0
		AND NOT EXISTS (SELECT 1 FROM outbox_recipient_progress p WHERE p.outbox_id=outbox.id)`,
		"管理员核对中继记录后确认未投递。", id, input.Attempts)
	err = requireAffectedRow(result, err)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusConflict, "任务状态已变化，请刷新队列")
		return
	}
	if letterError(w, err) {
		return
	}
	var sender string
	if err := tx.QueryRowContext(ctx, `SELECT sender FROM outbox WHERE id=?`, id).Scan(&sender); letterError(w, err) {
		return
	}
	action := "delivery_rejected"
	if input.Outcome == "accepted" {
		action = "delivery_confirmed"
		if letterError(w, a.confirmUncertainRelayDelivery(ctx, tx, id)) {
			return
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_delivery_resolutions(job_id,actor,sender,action,stamp)
		VALUES(?,?,?,?,?)`, id, actor, sender, action, time.Now().Unix())
	if !letterError(w, err) && !letterError(w, a.commitMailbox(tx, sender)) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *app) confirmUncertainRelayDelivery(ctx context.Context, tx *sql.Tx, id int64) error {
	var m letter
	var raw []byte
	var encoded string
	var localDone bool
	err := tx.QueryRowContext(ctx, `SELECT o.sender,o.recipient,o.subject,o.body,o.stamp,o.raw,
		COALESCE(e.envelope,''),COALESCE(e.local_done,FALSE)
		FROM outbox o LEFT JOIN web_outbox_envelopes e ON e.outbox_id=o.id WHERE o.id=?`, id).
		Scan(&m.From, &m.To, &m.Subject, &m.Body, &m.Date, &raw, &encoded, &localDone)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox_schedule WHERE outbox_id=?`, id); err != nil {
		return err
	}
	if err := deleteWebEnvelope(ctx, tx, id); err != nil {
		return err
	}
	if localDone {
		return nil
	}
	envelope, err := parseWebEnvelope(encoded)
	if err != nil {
		return err
	}
	if envelope != nil {
		m.recipients = &envelope.Recipients
	}
	return a.insertQueuedSent(ctx, tx, m, raw)
}
