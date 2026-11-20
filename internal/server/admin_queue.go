package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const adminQueuePageSize = 30

type adminQueueItem struct {
	Tracked  bool   `json:"tracked"`
	ID       int64  `json:"id,string"`
	Sender   string `json:"sender"`
	Stamp    int64  `json:"stamp"`
	State    string `json:"state"`
	Attempts int    `json:"attempts"`
	Due      int64  `json:"due"`
	Reason   string `json:"reason,omitempty"`
	CanRetry bool   `json:"canRetry"`
}

type adminRetryRecord struct {
	JobID       int64  `json:"jobId,string"`
	Actor       string `json:"actor"`
	Sender      string `json:"sender"`
	PreviousDue int64  `json:"previousDue"`
	Stamp       int64  `json:"stamp"`
}

func (a *app) adminQueue(w http.ResponseWriter, r *http.Request, owner string) {
	params := r.URL.Query()
	state := params.Get("state")
	switch state {
	case "", "queued", "scheduled", "undo", "preparing", "sending", "failed", "uncertain":
	default:
		fail(w, 400, "无效的投递状态")
		return
	}
	sender := strings.TrimSpace(params.Get("sender"))
	if len(sender) > 254 {
		fail(w, 400, "发件地址过长")
		return
	}
	var filters strings.Builder
	var args []any
	if state != "" {
		filters.WriteString(" AND state=?")
		args = append(args, state)
	}
	if sender != "" {
		filters.WriteString(" AND instr(sender,?)>0")
		args = append(args, strings.ToLower(sender))
	}
	var since, until int64
	for _, entry := range []struct {
		name, clause string
		target       *int64
	}{
		{"since", " AND stamp>=?", &since}, {"until", " AND stamp<?", &until}, {"before", " AND id<?", nil}, {"id", " AND id=?", nil},
	} {
		if value := params.Get(entry.name); value != "" {
			number, err := strconv.ParseInt(value, 10, 64)
			if err != nil || number <= 0 {
				fail(w, 400, "日期或分页参数无效")
				return
			}
			if entry.target != nil {
				*entry.target = number
			}
			filters.WriteString(entry.clause)
			args = append(args, number)
		}
	}
	if until != 0 && since >= until {
		fail(w, 400, "起始时间必须早于结束时间")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	items, err := a.readAdminQueue(ctx, filters.String(), args, adminQueuePageSize+1)
	if err != nil {
		fail(w, 503, "无法读取投递队列，请稍后刷新")
		return
	}
	audit, err := a.readAdminRetryRecords(ctx)
	if err != nil {
		fail(w, 503, "无法读取重试记录，请稍后刷新")
		return
	}
	var next string
	if len(items) > adminQueuePageSize {
		items = items[:adminQueuePageSize]
		next = strconv.FormatInt(items[len(items)-1].ID, 10)
	}
	reply(w, 200, map[string]any{"email": owner, "items": items, "next": next, "retries": audit})
}

func (a *app) readAdminQueue(ctx context.Context, filters string, args []any, limit int) ([]adminQueueItem, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT id,sender,stamp,state,attempts,due,reason,
		EXISTS(SELECT 1 FROM outbox_recipient_progress p WHERE p.outbox_id=outbox.id) FROM outbox WHERE 1=1`+
		filters+` ORDER BY id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]adminQueueItem, 0, limit)
	now := time.Now().Unix()
	for rows.Next() {
		var item adminQueueItem
		if err := rows.Scan(&item.ID, &item.Sender, &item.Stamp, &item.State, &item.Attempts, &item.Due, &item.Reason, &item.Tracked); err != nil {
			return nil, err
		}
		item.CanRetry = a.canRetryDelivery(item, now)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (a *app) canRetryDelivery(item adminQueueItem, now int64) bool {
	return a.hasRelayFor(item.Sender) && item.State == "queued" && item.Attempts > 0 && item.Reason != "" && item.Due > now
}

func (a *app) readAdminRetryRecords(ctx context.Context) ([]adminRetryRecord, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT job_id,actor,sender,previous_due,stamp FROM admin_delivery_retries ORDER BY id DESC LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := make([]adminRetryRecord, 0, 20)
	for rows.Next() {
		var record adminRetryRecord
		if err := rows.Scan(&record.JobID, &record.Actor, &record.Sender, &record.PreviousDue, &record.Stamp); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (a *app) adminRetryDelivery(w http.ResponseWriter, r *http.Request, actor string) {
	id := letterID(r)
	var expected struct {
		Due      int64 `json:"due"`
		Attempts int   `json:"attempts"`
	}
	if !decode(w, r, &expected) {
		return
	}
	if id == 0 || expected.Due <= 0 || expected.Attempts < 1 {
		fail(w, 400, "重试请求缺少任务版本")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if letterError(w, err) {
		return
	}
	defer tx.Rollback()
	var sender string
	if err := tx.QueryRowContext(ctx, `SELECT sender FROM outbox WHERE id=?`, id).Scan(&sender); errors.Is(err, sql.ErrNoRows) {
		fail(w, 409, "任务已变化或已到期，请刷新队列")
		return
	} else if letterError(w, err) {
		return
	}
	if !a.hasRelayFor(sender) {
		fail(w, 409, "发件域尚未配置外发中继")
		return
	}
	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx, `UPDATE outbox SET due=? WHERE id=? AND state='queued'
		AND due=? AND due>? AND attempts=? AND reason<>'' AND claim='' AND lease=0`,
		now, id, expected.Due, now, expected.Attempts)
	err = requireAffectedRow(result, err)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, 409, "任务已变化或已到期，请刷新队列")
		return
	}
	if letterError(w, err) {
		return
	}
	if _, err := tx.ExecContext(ctx, `UPDATE outbox_recipient_progress SET due=? WHERE outbox_id=? AND state='queued' AND due>?`,
		now, id, now); letterError(w, err) {
		return
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_delivery_retries(job_id,actor,sender,previous_due,stamp)
		VALUES(?,?,?,?,?)`, id, actor, sender, expected.Due, now); letterError(w, err) {
		return
	}
	if !letterError(w, a.commitOutbox(tx, sender)) {
		w.WriteHeader(http.StatusNoContent)
	}
}
