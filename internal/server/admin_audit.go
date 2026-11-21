package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/csv"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const adminAuditPageSize = 30
const adminAuditExportLimit = 2000

type adminAuditEvent struct {
	ID          int64  `json:"-"`
	Source      int    `json:"-"`
	Stamp       int64  `json:"stamp"`
	Actor       string `json:"actor"`
	Target      string `json:"target"`
	Action      string `json:"action"`
	Affected    int64  `json:"affected"`
	JobID       int64  `json:"jobId,string,omitempty"`
	PreviousDue int64  `json:"previousDue,omitempty"`
	Alias       string `json:"alias,omitempty"`
}

type adminAuditFilter struct {
	Actor, Target, Action string
	Since, Until          int64
	Before                adminAuditEvent
}

func parseAdminAuditFilter(params url.Values) (adminAuditFilter, error) {
	filter := adminAuditFilter{
		Actor:  strings.ToLower(strings.TrimSpace(params.Get("actor"))),
		Target: strings.ToLower(strings.TrimSpace(params.Get("target"))),
		Action: params.Get("action"),
	}
	if len(filter.Actor) > 254 || len(filter.Target) > 254 {
		return filter, errors.New("审计筛选条件过长")
	}
	switch filter.Action {
	case "", "sessions", "app_passwords", "suspend", "restore", "retry", "alias_add", "alias_remove", "alias_send_grant", "alias_send_revoke", "password_reset", "password_change", "admin_grant", "admin_revoke", "outbound_limit", "account_create",
		"domain_add", "domain_remove", "dkim_generate", "dkim_enable", "dkim_disable", "storage_alert", "storage_quota", "delivery_confirmed", "delivery_rejected",
		"mailbox_retire", "mailbox_restore", "mailbox_purge", "mailbox_rename", "recipient_confirmed", "recipient_rejected", "quarantine_release", "quarantine_discard":
	default:
		return filter, errors.New("无效的审计操作类型")
	}
	for _, entry := range []struct {
		name string
		dest *int64
	}{{"since", &filter.Since}, {"until", &filter.Until}} {
		if raw := params.Get(entry.name); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value <= 0 {
				return filter, errors.New("审计日期无效")
			}
			*entry.dest = value
		}
	}
	if filter.Until != 0 && filter.Since >= filter.Until {
		return filter, errors.New("起始时间必须早于截止时间")
	}
	if cursor := params.Get("before"); cursor != "" {
		parts := strings.Split(cursor, ":")
		if len(parts) != 3 {
			return filter, errors.New("无效的审计分页参数")
		}
		stamp, p1 := strconv.ParseInt(parts[0], 10, 64)
		source, p2 := strconv.Atoi(parts[1])
		id, p3 := strconv.ParseInt(parts[2], 10, 64)
		if p1 != nil || p2 != nil || p3 != nil || stamp <= 0 || id <= 0 || source < 1 || source > 4 {
			return filter, errors.New("无效的审计分页参数")
		}
		filter.Before = adminAuditEvent{Stamp: stamp, Source: source, ID: id}
	}
	return filter, nil
}

func (e adminAuditEvent) cursor() string {
	return strconv.FormatInt(e.Stamp, 10) + ":" + strconv.Itoa(e.Source) + ":" + strconv.FormatInt(e.ID, 10)
}

type adminAuditSource struct {
	kind      int
	target    string
	selectSQL string
}

func (a *app) readAdminAudit(ctx context.Context, filter adminAuditFilter, limit int) ([]adminAuditEvent, error) {
	sources := []adminAuditSource{
		{1, "target", `SELECT id,stamp,actor,target,action,affected,0,0,'' FROM admin_account_actions WHERE 1=1`},
		{2, "sender", `SELECT id,stamp,actor,sender,'retry',0,job_id,previous_due,'' FROM admin_delivery_retries WHERE 1=1`},
		{3, "target", `SELECT id,stamp,actor,target,action,0,0,0,alias FROM admin_alias_actions WHERE 1=1`},
		{4, "sender", `SELECT id,stamp,actor,sender,action,0,job_id,0,'' FROM admin_delivery_resolutions WHERE 1=1`},
	}
	var only int
	switch filter.Action {
	case "":
	case "retry":
		only = 2
	case "alias_add", "alias_remove", "alias_send_grant", "alias_send_revoke":
		only = 3
	case "delivery_confirmed", "delivery_rejected", "recipient_confirmed", "recipient_rejected":
		only = 4
	default:
		only = 1
	}
	events := make([]adminAuditEvent, 0, limit)
	for _, source := range sources {
		if only != 0 && source.kind != only {
			continue
		}
		items, err := a.readAdminAuditSource(ctx, source, filter, limit)
		if err != nil {
			return nil, err
		}
		events = append(events, items...)
	}
	slices.SortFunc(events, func(left, right adminAuditEvent) int {
		if order := cmp.Compare(right.Stamp, left.Stamp); order != 0 {
			return order
		}
		if order := cmp.Compare(right.Source, left.Source); order != 0 {
			return order
		}
		return cmp.Compare(right.ID, left.ID)
	})
	return events[:min(len(events), limit)], nil
}

func (a *app) readAdminAuditSource(ctx context.Context, source adminAuditSource, f adminAuditFilter, limit int) ([]adminAuditEvent, error) {
	query := source.selectSQL
	var args []any
	if f.Actor != "" {
		query += " AND instr(actor,?)>0"
		args = append(args, f.Actor)
	}
	if f.Target != "" {
		query += " AND " + source.target + "=?"
		args = append(args, f.Target)
	}
	if f.Action != "" && source.kind != 2 {
		query += " AND action=?"
		args = append(args, f.Action)
	}
	if f.Since != 0 {
		query += " AND stamp>=?"
		args = append(args, f.Since)
	}
	if f.Until != 0 {
		query += " AND stamp<?"
		args = append(args, f.Until)
	}
	if f.Before.ID != 0 {
		switch {
		case source.kind < f.Before.Source:
			query += " AND stamp<=?"
			args = append(args, f.Before.Stamp)
		case source.kind == f.Before.Source:
			query += " AND (stamp<? OR (stamp=? AND id<?))"
			args = append(args, f.Before.Stamp, f.Before.Stamp, f.Before.ID)
		default:
			query += " AND stamp<?"
			args = append(args, f.Before.Stamp)
		}
	}
	rows, err := a.db.QueryContext(ctx, query+" ORDER BY stamp DESC,id DESC LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]adminAuditEvent, 0)
	for rows.Next() {
		event := adminAuditEvent{Source: source.kind}
		if err := rows.Scan(&event.ID, &event.Stamp, &event.Actor, &event.Target,
			&event.Action, &event.Affected, &event.JobID, &event.PreviousDue, &event.Alias); err != nil {
			return nil, err
		}
		items = append(items, event)
	}
	return items, rows.Err()
}

func (a *app) adminAudit(w http.ResponseWriter, r *http.Request, actor string) {
	filter, err := parseAdminAuditFilter(r.URL.Query())
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	events, err := a.readAdminAudit(ctx, filter, adminAuditPageSize+1)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "无法读取审计记录，请稍后刷新")
		return
	}
	var next string
	if len(events) > adminAuditPageSize {
		events = events[:adminAuditPageSize]
		next = events[len(events)-1].cursor()
	}
	reply(w, http.StatusOK, map[string]any{"email": actor, "events": events, "next": next})
}

func (a *app) adminAuditExport(w http.ResponseWriter, r *http.Request, _ string) {
	filter, err := parseAdminAuditFilter(r.URL.Query())
	if err != nil || filter.Before.ID != 0 {
		fail(w, http.StatusBadRequest, "导出筛选条件无效")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	events, err := a.readAdminAudit(ctx, filter, adminAuditExportLimit+1)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "无法导出审计记录，请稍后重试")
		return
	}
	if len(events) > adminAuditExportLimit {
		fail(w, http.StatusUnprocessableEntity, "导出超过 2000 条，请缩小日期或筛选范围")
		return
	}
	var buffer bytes.Buffer
	buffer.WriteString("\ufeff")
	writer := csv.NewWriter(&buffer)
	writer.UseCRLF = true
	_ = writer.Write([]string{"操作时间（UTC）", "操作者", "目标", "操作", "撤销记录数", "投递任务编号", "原重试时间（UTC）", "别名"})
	for _, event := range events {
		var job, previousDue, affected string
		switch event.Action {
		case "retry":
			job = strconv.FormatInt(event.JobID, 10)
			previousDue = time.Unix(event.PreviousDue, 0).UTC().Format(time.RFC3339)
		case "delivery_confirmed", "delivery_rejected", "recipient_confirmed", "recipient_rejected":
			job = strconv.FormatInt(event.JobID, 10)
		case "restore", "alias_add", "alias_remove", "alias_send_grant", "alias_send_revoke", "account_create", "domain_add", "domain_remove", "dkim_generate", "dkim_enable", "dkim_disable", "storage_alert", "storage_quota",
			"mailbox_retire", "mailbox_restore", "mailbox_purge", "mailbox_rename":
		default:
			affected = strconv.FormatInt(event.Affected, 10)
		}
		_ = writer.Write([]string{time.Unix(event.Stamp, 0).UTC().Format(time.RFC3339), csvAuditText(event.Actor),
			csvAuditText(event.Target), csvAuditText(event.Action), affected, job, previousDue, csvAuditText(event.Alias)})
	}
	writer.Flush()
	if writer.Error() != nil {
		fail(w, http.StatusInternalServerError, "无法生成审计文件")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="harbor-mail-audit.csv"`)
	_, _ = w.Write(buffer.Bytes())
}

func csvAuditText(value string) string {
	s := strings.TrimLeftFunc(value, unicode.IsSpace)
	if len(s) != 0 && strings.ContainsRune("=+-@", rune(s[0])) {
		return "'" + value
	}
	return value
}
