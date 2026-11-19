package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxMailRules = 50

var (
	errRuleInput  = errors.New("invalid mail rule")
	errRuleLimit  = errors.New("mail rule limit reached")
	errRuleFolder = errors.New("rule destination not available")
	errRuleLabel  = errors.New("rule label not available")
	domainLabel   = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

type mailRule struct {
	ID          int64  `json:"id"`
	Order       int    `json:"order"`
	Enabled     bool   `json:"enabled"`
	Field       string `json:"field"`
	Value       string `json:"value"`
	Action      string `json:"action"`
	Destination string `json:"destination"`
	LabelID     int64  `json:"labelID,omitempty"`
}

func (rule *mailRule) validate() error {
	rule.Value = strings.TrimSpace(rule.Value)
	if rule.Order < 1 || rule.Order > 1000 || rule.Value == "" ||
		utf8.RuneCountInString(rule.Value) > 254 || strings.ContainsFunc(rule.Value, unicode.IsControl) {
		return errRuleInput
	}
	switch rule.Field {
	case "sender", "recipient":
		parsed, err := address(rule.Value)
		if err != nil || !strings.EqualFold(parsed, rule.Value) {
			return errRuleInput
		}
	case "domain":
		rule.Value = strings.ToLower(rule.Value)
		if len(rule.Value) > 253 {
			return errRuleInput
		}
		for _, label := range strings.Split(rule.Value, ".") {
			if !domainLabel.MatchString(label) {
				return errRuleInput
			}
		}
	case "subject":
	default:
		return errRuleInput
	}
	switch rule.Action {
	case "move":
		rule.LabelID = 0
		if rule.Destination != "archive" && rule.Destination != "junk" &&
			rule.Destination != "inbox" && !customFolderID.MatchString(rule.Destination) {
			return errRuleFolder
		}
	case "star", "read":
		rule.Destination = ""
		rule.LabelID = 0
	case "label":
		if rule.LabelID < 1 {
			return errRuleLabel
		}
		rule.Destination = ""
	default:
		return errRuleInput
	}
	return nil
}

type rulesQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readMailRules(ctx context.Context, db rulesQueryer, owner string, enabledOnly bool) ([]mailRule, error) {
	query := `SELECT id,rule_order,enabled,match_field,match_value,rule_action,destination FROM mail_rules WHERE owner=?`
	if enabledOnly {
		query += ` AND enabled=TRUE`
	}
	rows, err := db.QueryContext(ctx, query+` ORDER BY rule_order,id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rules := make([]mailRule, 0)
	for rows.Next() {
		var rule mailRule
		if err := rows.Scan(&rule.ID, &rule.Order, &rule.Enabled, &rule.Field, &rule.Value, &rule.Action, &rule.Destination); err != nil {
			return nil, err
		}
		if rule.Action == "label" {
			rule.LabelID, _ = strconv.ParseInt(rule.Destination, 36, 64)
			rule.Destination = ""
		}
		rules = append(rules, rule)
	}
	return rules, rows.Err()
}

func (rule mailRule) matches(sender, domain, subject, recipient string) bool {
	switch rule.Field {
	case "sender":
		return strings.EqualFold(rule.Value, sender)
	case "recipient":
		return strings.EqualFold(rule.Value, recipient)
	case "domain":
		return strings.EqualFold(rule.Value, domain)
	case "subject":
		return strings.Contains(subject, strings.ToLower(rule.Value))
	}
	return false
}

// Callers hold the mailbox write lock, which also protects rule targets from deletion.
func insertIncoming(ctx context.Context, tx *sql.Tx, owner, sender, folder string, m letter, raw []byte) error {
	rules, err := readMailRules(ctx, tx, owner, true)
	if err != nil {
		return err
	}
	domain := ""
	if at := strings.LastIndexByte(sender, '@'); at >= 0 {
		domain = sender[at+1:]
	}
	subject := strings.ToLower(m.Subject)
	var label int64
	for _, rule := range rules {
		if !rule.matches(sender, domain, subject, m.To) {
			continue
		}
		switch rule.Action {
		case "move":
			folder = rule.Destination
		case "star":
			m.Star = true
		case "read":
			m.Read = true
		case "label":
			label = rule.LabelID
		}
		break
	}
	id, err := insertLetter(tx, owner, m, folder, "inbox", raw)
	if err != nil || label == 0 {
		return err
	}
	return attachMailLabel(ctx, tx, id, label)
}

func (a *app) saveMailRule(ctx context.Context, owner string, rule mailRule) (mailRule, error) {
	if err := rule.validate(); err != nil {
		return rule, err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return rule, err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return rule, err
	}
	target := rule.Destination
	switch rule.Action {
	case "move":
		if err := verifyFolder(ctx, tx, owner, target); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return rule, errRuleFolder
			}
			return rule, err
		}
	case "label":
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM mail_labels WHERE owner=? AND id=?`, owner, rule.LabelID).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return rule, errRuleLabel
			}
			return rule, err
		}
		// Base 36 fits a BIGINT label ID in the existing 16-character destination column.
		target = strconv.FormatInt(id, 36)
	}
	if rule.ID == 0 {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_rules WHERE owner=?`, owner).Scan(&count); err != nil {
			return rule, err
		}
		if count >= maxMailRules {
			return rule, errRuleLimit
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO mail_rules
			(owner,rule_order,enabled,match_field,match_value,rule_action,destination) VALUES(?,?,?,?,?,?,?)`,
			owner, rule.Order, rule.Enabled, rule.Field, rule.Value, rule.Action, target)
		if err != nil {
			return rule, err
		}
		rule.ID, err = result.LastInsertId()
		if err != nil {
			return rule, err
		}
	} else {
		result, err := tx.ExecContext(ctx, `UPDATE mail_rules SET rule_order=?,enabled=?,match_field=?,match_value=?,
			rule_action=?,destination=? WHERE owner=? AND id=?`,
			rule.Order, rule.Enabled, rule.Field, rule.Value, rule.Action, target, owner, rule.ID)
		if err := requireAffectedRow(result, err); err != nil {
			return rule, err
		}
	}
	return rule, tx.Commit()
}

func ruleHTTPError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errRuleInput):
		fail(w, 400, "请检查匹配条件和优先级")
	case errors.Is(err, errRuleFolder):
		fail(w, 400, "请选择收件箱、归档、垃圾邮件或你自己的文件夹")
	case errors.Is(err, errRuleLabel):
		fail(w, 400, "请选择你自己的邮件标签")
	case errors.Is(err, errRuleLimit):
		fail(w, 409, "每个邮箱最多可保存 50 条规则")
	case errors.Is(err, sql.ErrNoRows):
		fail(w, 404, "找不到这条规则")
	default:
		log.Print("mail rule operation: ", err)
		fail(w, 500, "规则操作失败")
	}
	return true
}

func (a *app) listRulesHTTP(w http.ResponseWriter, r *http.Request, owner string) {
	rules, err := readMailRules(r.Context(), a.db, owner, false)
	if !ruleHTTPError(w, err) {
		reply(w, 200, rules)
	}
}

func (a *app) saveRuleHTTP(w http.ResponseWriter, r *http.Request, owner string) {
	var rule mailRule
	if !decode(w, r, &rule) {
		return
	}
	rule.ID = 0
	status := 201
	if r.Method == http.MethodPut {
		rule.ID, status = letterID(r), 200
		if rule.ID == 0 {
			ruleHTTPError(w, sql.ErrNoRows)
			return
		}
	}
	rule, err := a.saveMailRule(r.Context(), owner, rule)
	if !ruleHTTPError(w, err) {
		reply(w, status, rule)
	}
}

func (a *app) deleteRuleHTTP(w http.ResponseWriter, r *http.Request, owner string) {
	result, err := a.db.ExecContext(r.Context(), `DELETE FROM mail_rules WHERE owner=? AND id=?`, owner, letterID(r))
	if !ruleHTTPError(w, requireAffectedRow(result, err)) {
		w.WriteHeader(204)
	}
}
