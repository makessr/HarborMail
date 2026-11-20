package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const loginAuditPage = 100

type loginEvent struct {
	ID       int64  `json:"id,string"`
	At       int64  `json:"at"`
	Source   string `json:"source"`
	Account  string `json:"account,omitempty"`
	Result   string `json:"result"`
	Protocol string `json:"protocol"`
}

func loginAuditRetention() (int, error) {
	days, err := strconv.Atoi(env("HARBOR_LOGIN_AUDIT_DAYS", "30"))
	if err != nil || days < 1 || days > 90 {
		return 0, errors.New("HARBOR_LOGIN_AUDIT_DAYS must be an integer from 1 to 90")
	}
	return days, nil
}

func (a *app) loginAuditDays() int {
	if a.auditDays == 0 {
		return 30
	}
	return a.auditDays
}

func (a *app) recordLogin(source netip.Prefix, account, result, protocol string) {
	if a.recoveryMode {
		return
	}
	value := "未知"
	if source.IsValid() {
		value = source.String()
		if source.Addr().Is4() {
			value = source.Addr().String()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	now := time.Now().Unix()
	_, err := a.db.ExecContext(ctx, `INSERT INTO login_audit_events(stamp,source,account,protocol,result)
		VALUES(?,?,?,?,?)`, now, value, account, protocol, result)
	if err != nil {
		last := a.auditLogAt.Load()
		if now-last >= 60 && a.auditLogAt.CompareAndSwap(last, now) {
			log.Print("Could not save login audit event")
		}
	}
}

func (a *app) pruneLoginAudit(ctx context.Context, cutoff int64) error {
	for {
		result, err := a.db.ExecContext(ctx, `DELETE FROM login_audit_events WHERE id IN (
			SELECT id FROM (SELECT id FROM login_audit_events WHERE stamp < ? ORDER BY stamp,id LIMIT 1000) AS expired
		)`, cutoff)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count < 1000 {
			return err
		}
	}
}

func (a *app) runLoginAudit(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		sweep, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := a.pruneLoginAudit(sweep, time.Now().Add(-time.Duration(a.loginAuditDays())*24*time.Hour).Unix())
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Print("Could not prune expired login audit events")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func loginAuditAccount(value string) string {
	if len(value) > 254 {
		return ""
	}
	account, _ := address(strings.ToLower(strings.TrimSpace(value)))
	return account
}

func (a *app) adminSecurity(w http.ResponseWriter, r *http.Request, actor string) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	now := time.Now()
	cutoff := now.Add(-time.Duration(a.loginAuditDays()) * 24 * time.Hour).Unix()
	filters := []string{"stamp>=?"}
	args := []any{cutoff}
	for _, filter := range []struct {
		key, column string
	}{
		{"account", "account=?"}, {"source", "source=?"}, {"protocol", "protocol=?"}, {"result", "result=?"},
		{"from", "stamp>=?"}, {"until", "stamp<?"}, {"before", "id<?"},
	} {
		value := strings.TrimSpace(q.Get(filter.key))
		if value == "" {
			continue
		}
		var bound any = value
		switch filter.key {
		case "account":
			normalized := loginAuditAccount(value)
			if normalized == "" {
				fail(w, 400, "请输入有效的邮箱地址")
				return
			}
			bound = normalized
		case "source":
			if len(value) > 48 {
				fail(w, 400, "来源地址无效")
				return
			}
		case "protocol":
			switch value {
			case "web", "smtp", "imap", "pop3":
			default:
				fail(w, 400, "登录协议无效")
				return
			}
		case "result":
			switch value {
			case "success", "failed", "blocked", "busy", "error":
			default:
				fail(w, 400, "登录结果无效")
				return
			}
		default:
			number, err := strconv.ParseInt(value, 10, 64)
			if err != nil || number <= 0 {
				fail(w, 400, "时间或分页参数无效")
				return
			}
			bound = number
		}
		filters = append(filters, filter.column)
		args = append(args, bound)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	totals, err := a.loginAuditTotals(ctx, cutoff)
	if err != nil {
		fail(w, 503, "暂时无法读取登录审计")
		return
	}
	rows, err := a.db.QueryContext(ctx, `SELECT id,stamp,source,account,protocol,result FROM login_audit_events WHERE `+
		strings.Join(filters, " AND ")+` ORDER BY id DESC LIMIT ?`, append(args, loginAuditPage+1)...)
	if err != nil {
		fail(w, 503, "暂时无法读取登录审计")
		return
	}
	defer rows.Close()
	events := make([]loginEvent, 0, loginAuditPage+1)
	for rows.Next() {
		var event loginEvent
		if err := rows.Scan(&event.ID, &event.At, &event.Source, &event.Account, &event.Protocol, &event.Result); err != nil {
			fail(w, 503, "暂时无法读取登录审计")
			return
		}
		events = append(events, event)
	}
	if rows.Err() != nil {
		fail(w, 503, "暂时无法读取登录审计")
		return
	}
	hasMore := len(events) > loginAuditPage
	if hasMore {
		events = events[:loginAuditPage]
	}
	g := &a.webLogin
	reply(w, http.StatusOK, map[string]any{
		"email": actor, "events": events, "totals": totals, "windowSeconds": int(loginWindow.Seconds()),
		"sourceLimit": loginSourceLimit, "pairLimit": loginPairLimit, "trustedProxies": len(g.proxies),
		"sharedLimit": g.shared != nil, "retentionDays": a.loginAuditDays(), "hasMore": hasMore,
	})
}

func (a *app) loginAuditTotals(ctx context.Context, cutoff int64) (map[string]int64, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT result,COUNT(*) FROM login_audit_events WHERE stamp>=? GROUP BY result`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	totals := make(map[string]int64)
	for rows.Next() {
		var result string
		var count int64
		if err := rows.Scan(&result, &count); err != nil {
			return nil, err
		}
		totals[result] = count
	}
	return totals, rows.Err()
}
