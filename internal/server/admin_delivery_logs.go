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

var errOutboundLimit = errors.New("daily outbound recipient limit reached")

type outboundLimit struct {
	Scope  string `json:"scope"`
	Target string `json:"target"`
	Limit  int    `json:"limit"`
	Used   int    `json:"used"`
}

func consumeOutboundLimit(ctx context.Context, tx *sql.Tx, owner string, count int, now time.Time) error {
	if count == 0 {
		return nil
	}
	day := now.Unix() / 86400
	for _, limit := range []outboundLimit{{Scope: "account", Target: owner}, {Scope: "domain", Target: emailDomain(owner)}} {
		var configured int
		err := tx.QueryRowContext(ctx, "SELECT daily_limit FROM outbound_limits WHERE scope=? AND target=?", limit.Scope, limit.Target).Scan(&configured)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && configured == 0) {
			continue
		}
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE outbound_limits
			SET used=CASE WHEN day=? THEN used+? ELSE ? END, day=?
			WHERE scope=? AND target=? AND (daily_limit=0 OR daily_limit >= CASE WHEN day=? THEN used+? ELSE ? END)`,
			day, count, count, day, limit.Scope, limit.Target, day, count, count)
		if err := requireAffectedRow(result, err); errors.Is(err, sql.ErrNoRows) {
			return errOutboundLimit
		} else if err != nil {
			return err
		}
	}
	return nil
}

func (a *app) adminOutboundLimits(w http.ResponseWriter, r *http.Request, _ string) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT scope,target,daily_limit,CASE WHEN day=? THEN used ELSE 0 END
		FROM outbound_limits ORDER BY scope,target`, time.Now().Unix()/86400)
	if letterError(w, err) {
		return
	}
	defer rows.Close()
	limits := make([]outboundLimit, 0)
	for rows.Next() {
		var item outboundLimit
		if letterError(w, rows.Scan(&item.Scope, &item.Target, &item.Limit, &item.Used)) {
			return
		}
		limits = append(limits, item)
	}
	if !letterError(w, rows.Err()) {
		reply(w, http.StatusOK, limits)
	}
}

func (a *app) adminSetOutboundLimit(w http.ResponseWriter, r *http.Request, actor string) {
	var input struct {
		outboundLimit
		Password string
	}
	if !decode(w, r, &input) {
		return
	}
	input.Target = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(input.Target)), ".")
	if input.Scope != "domain" && input.Scope != "account" || input.Limit < 0 || input.Limit > 1_000_000 {
		fail(w, http.StatusBadRequest, "请选择账号或域名，并输入 0 至 1000000 的外发额度")
		return
	}
	if input.Scope == "account" {
		if canonical, err := address(input.Target); err != nil || canonical != input.Target {
			fail(w, http.StatusBadRequest, "请输入完整主邮箱地址")
			return
		}
	} else if !validDNSName(input.Target) || !strings.Contains(input.Target, ".") {
		fail(w, http.StatusBadRequest, "请输入完整邮件域名")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	if _, _, err := a.authenticatePrimary(ctx, actor, input.Password); err != nil {
		if !loginTemporaryError(w, err) {
			fail(w, http.StatusForbidden, "管理员主邮箱密码不正确")
		}
		return
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if letterError(w, err) {
		return
	}
	defer tx.Rollback()
	if letterError(w, lockIMAPUIDs(ctx, tx)) ||
		browserSessionError(w, a.lockBrowserSession(ctx, tx, actor, browserSessionDigest(r))) {
		return
	}
	if input.Scope == "account" && (input.Limit != 0 || a.publicDefaults) {
		if _, err := a.baseAccountHash(ctx, tx, input.Target); letterError(w, err) {
			return
		}
	}
	if input.Limit == 0 && !a.publicDefaults {
		_, err = tx.ExecContext(ctx, "DELETE FROM outbound_limits WHERE scope=? AND target=?", input.Scope, input.Target)
	} else {
		storage := `INSERT INTO outbound_limits(scope,target,daily_limit,day,used) VALUES(?,?,?,0,0)
			ON CONFLICT(scope,target) DO UPDATE SET daily_limit=excluded.daily_limit`
		if a.dbDriver == "mysql" {
			storage = `INSERT INTO outbound_limits(scope,target,daily_limit,day,used) VALUES(?,?,?,0,0)
				ON DUPLICATE KEY UPDATE daily_limit=?`
			_, err = tx.ExecContext(ctx, storage, input.Scope, input.Target, input.Limit, input.Limit)
		} else {
			_, err = tx.ExecContext(ctx, storage, input.Scope, input.Target, input.Limit)
		}
	}
	if letterError(w, err) {
		return
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_account_actions(actor,target,action,affected,stamp)
		VALUES(?,?,?,?,?)`, actor, input.Target, "outbound_limit", input.Limit, time.Now().Unix()); letterError(w, err) {
		return
	}
	if !letterError(w, tx.Commit()) {
		w.WriteHeader(http.StatusNoContent)
	}
}

type adminDeliveryEvent struct {
	ID     int64  `json:"id"`
	Outbox int64  `json:"outbox"`
	Owner  string `json:"owner"`
	deliveryEvent
}

func (a *app) adminDeliveryLogs(w http.ResponseWriter, r *http.Request, _ string) {
	params := r.URL.Query()
	query := "SELECT id,outbox_id,owner,recipient,state,stamp,due,smtp_code FROM delivery_events WHERE stamp>=?"
	args := []any{time.Now().Add(-90 * 24 * time.Hour).Unix()}
	for _, filter := range []struct{ parameter, column string }{{"owner", "owner"}, {"recipient", "recipient"}, {"state", "state"}} {
		value := strings.TrimSpace(params.Get(filter.parameter))
		if len(value) > 254 {
			fail(w, http.StatusBadRequest, "筛选条件过长")
			return
		}
		if value != "" {
			query += " AND " + filter.column + "=?"
			args = append(args, strings.ToLower(value))
		}
	}
	if value := params.Get("before"); value != "" {
		cursor, err := strconv.ParseInt(value, 10, 64)
		if err != nil || cursor < 1 {
			fail(w, http.StatusBadRequest, "无效的日志游标")
			return
		}
		query += " AND id<?"
		args = append(args, cursor)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := a.db.QueryContext(ctx, query+" ORDER BY id DESC LIMIT 101", args...)
	if letterError(w, err) {
		return
	}
	defer rows.Close()
	events := make([]adminDeliveryEvent, 0, 100)
	var next int64
	for rows.Next() {
		var event adminDeliveryEvent
		if letterError(w, rows.Scan(&event.ID, &event.Outbox, &event.Owner, &event.Address, &event.State, &event.At, &event.Due, &event.Code)) {
			return
		}
		if len(events) == 100 {
			next = events[len(events)-1].ID
			break
		}
		events = append(events, event)
	}
	if !letterError(w, rows.Err()) {
		reply(w, http.StatusOK, map[string]any{"events": events, "next": next})
	}
}
