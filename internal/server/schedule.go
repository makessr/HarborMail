package server

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	message "github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

var errScheduleStarted = errors.New("scheduled delivery already started")

type sendPlan struct {
	at          int64
	undoSeconds int
}

func validUndoDelay(seconds int) bool {
	return seconds == 5 || seconds == 10 || seconds == 20 || seconds == 30
}

func parseSendPlan(w http.ResponseWriter, r *http.Request) (sendPlan, bool) {
	params, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(w, http.StatusBadRequest, "发送时间格式无效")
		return sendPlan{}, false
	}
	values, present := params["sendAt"]
	if undo, enabled := params["undoSeconds"]; enabled {
		if present || len(undo) != 1 {
			fail(w, http.StatusBadRequest, "请选择一种发送方式")
			return sendPlan{}, false
		}
		seconds, err := strconv.Atoi(undo[0])
		if err != nil || !validUndoDelay(seconds) {
			fail(w, http.StatusBadRequest, "请选择有效的撤销发送等待时间")
			return sendPlan{}, false
		}
		return sendPlan{undoSeconds: seconds}, true
	}
	if !present {
		return sendPlan{}, true
	}
	if len(values) != 1 {
		fail(w, http.StatusBadRequest, "请选择一个发送时间")
		return sendPlan{}, false
	}
	stamp, err := strconv.ParseInt(values[0], 10, 64)
	latest := time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC).Unix()
	if err != nil || stamp <= time.Now().Unix() || stamp > latest {
		fail(w, http.StatusBadRequest, "请选择有效的未来发送时间")
		return sendPlan{}, false
	}
	return sendPlan{at: stamp}, true
}

func (p sendPlan) deferred() bool {
	return p.at != 0 || p.undoSeconds != 0
}

type scheduledSend struct {
	id, at int64
	undo   bool
}

func undoRemainingMs(at int64) int64 {
	return max(0, time.Unix(at, 0).UnixMilli()-time.Now().UnixMilli())
}

func (s scheduledSend) reply(w http.ResponseWriter) {
	payload := map[string]int64{"queued": s.id, "sendAt": s.at}
	if s.undo {
		payload["undoRemainingMs"] = undoRemainingMs(s.at)
	}
	reply(w, http.StatusAccepted, payload)
}

func scheduleLetter(ctx context.Context, tx *sql.Tx, m letter, raw []byte, localOwner string, plan sendPlan) (scheduledSend, error) {
	return scheduleDelivery(ctx, tx, m, raw, localOwner, nil, plan)
}

func scheduleDelivery(ctx context.Context, tx *sql.Tx, m letter, raw []byte, localOwner string, envelope *webEnvelope, plan sendPlan) (scheduledSend, error) {
	state := "scheduled"
	if plan.undoSeconds != 0 {
		state = "undo"
	}
	id, err := insertOutbox(tx, m, raw, state, plan.at)
	if err != nil {
		return scheduledSend{}, err
	}
	at := plan.at
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_schedule(outbox_id,send_at,local_owner) VALUES(?,?,?)`,
		id, at, localOwner); err != nil {
		return scheduledSend{}, err
	}
	if envelope != nil {
		if err := saveWebEnvelope(ctx, tx, id, *envelope); err != nil {
			return scheduledSend{}, err
		}
	}
	if plan.undoSeconds != 0 {
		// Queue and metadata rows are locked. Set the deadline as the final queue
		// write before commit, after MIME encoding and any wait for INSERT locks.
		at = time.Now().Unix() + int64(plan.undoSeconds) + 1
		if _, err := tx.ExecContext(ctx, `UPDATE outbox SET due=? WHERE id=?`, at, id); err != nil {
			return scheduledSend{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbox_schedule SET send_at=? WHERE outbox_id=?`, at, id); err != nil {
			return scheduledSend{}, err
		}
	}
	return scheduledSend{id: id, at: at, undo: plan.undoSeconds != 0}, nil
}

func (a *app) scheduleOutgoing(ctx context.Context, m letter, raw []byte, localOwner string, plan sendPlan) (scheduledSend, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return scheduledSend{}, err
	}
	defer tx.Rollback()
	queued, err := scheduleLetter(ctx, tx, m, raw, localOwner, plan)
	if err != nil {
		return scheduledSend{}, err
	}
	return queued, a.commitMailbox(tx, m.From)
}

func (a *app) deliverScheduledLocal(job *outboxJob) error {
	m := job.message
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return err
	}
	if err := a.checkDestination(ctx, tx, m.To, job.localOwner); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE id=? AND claim=? AND state='preparing' AND lease>?`,
		m.ID, job.claim, time.Now().Unix())
	if err := requireAffectedRow(result, err); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox_schedule WHERE outbox_id=?`, m.ID); err != nil {
		return err
	}
	if err := a.insertQueuedSent(ctx, tx, m, job.raw); err != nil {
		return err
	}
	if err := insertIncoming(ctx, tx, job.localOwner, m.From, "inbox", m, job.raw); err != nil {
		return err
	}
	return a.commitMailbox(tx, m.From, job.localOwner)
}

func (a *app) cancelScheduled(w http.ResponseWriter, r *http.Request, owner string) {
	draft, err := a.restoreScheduled(r.Context(), owner, letterID(r))
	if errors.Is(err, errScheduleStarted) {
		fail(w, http.StatusConflict, "撤销期限已过或邮件已开始投递")
		return
	}
	if !letterError(w, err) {
		reply(w, http.StatusOK, map[string]int64{"draft": draft})
	}
}

func (a *app) restoreScheduled(ctx context.Context, owner string, id int64) (int64, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return 0, err
	}
	m := letter{From: owner, Date: time.Now().Unix(), Read: true}
	var raw []byte
	var state, envelopeData string
	if err := tx.QueryRowContext(ctx, `SELECT recipient,subject,body,raw,state,coalesce(e.envelope,'')
		FROM outbox LEFT JOIN web_outbox_envelopes e ON e.outbox_id=id WHERE sender=? AND id=?`, owner, id).
		Scan(&m.To, &m.Subject, &m.Body, &raw, &state, &envelopeData); err != nil {
		return 0, err
	}
	if state != "scheduled" && state != "undo" {
		return 0, errScheduleStarted
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM outbox WHERE sender=? AND id=? AND state=?
		AND (state<>'undo' OR due>?)`, owner, id, state, time.Now().Unix())
	if err := requireAffectedRow(result, err); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, errScheduleStarted
		}
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox_schedule WHERE outbox_id=?`, id); err != nil {
		return 0, err
	}
	envelope, err := parseWebEnvelope(envelopeData)
	if err != nil {
		return 0, err
	}
	if envelope != nil {
		m.recipients = &envelope.Recipients
		if err := deleteWebEnvelope(ctx, tx, id); err != nil {
			return 0, err
		}
	}
	raw, err = rewriteMailDate(raw, m.Date)
	if err != nil {
		return 0, err
	}
	draft, err := insertWebDraft(ctx, tx, owner, m, raw)
	if err != nil {
		return 0, err
	}
	return draft, a.commitMailbox(tx, owner)
}

func rewriteMailDate(raw []byte, stamp int64) ([]byte, error) {
	return rewriteOutgoingHeaders(raw, stamp, nil)
}

func rewriteOutgoingHeaders(raw []byte, stamp int64, recipients *webRecipients) ([]byte, error) {
	reader := bufio.NewReader(bytes.NewReader(raw))
	header, err := textproto.ReadHeader(reader)
	if err != nil {
		return nil, err
	}
	header.Set("Date", time.Unix(stamp, 0).UTC().Format(time.RFC1123Z))
	if recipients != nil {
		header.Del("To")
		header.Del("Cc")
		header.Del("Bcc")
		header.Del("Resent-Bcc")
		var visible message.Header
		setVisibleRecipients(&visible, *recipients)
		for fields := visible.Fields(); fields.Next(); {
			header.Set(fields.Key(), fields.Value())
		}
	}
	var result bytes.Buffer
	result.Grow(len(raw))
	if err := textproto.WriteHeader(&result, header); err != nil {
		return nil, err
	}
	_, err = io.Copy(&result, reader)
	return result.Bytes(), err
}
