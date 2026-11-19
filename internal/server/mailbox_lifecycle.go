package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
)

var errMailboxRetired = errors.New("mailbox is retired")

func (a *app) retiredAccounts(ctx context.Context) (map[string]bool, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT owner FROM mailbox_retirements`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	owners := make(map[string]bool)
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, err
		}
		owners[owner] = true
	}
	return owners, rows.Err()
}

func (a *app) adminMailboxLifecycle(w http.ResponseWriter, r *http.Request, actor string) {
	owner, valid := storageAccountAddress(w, r)
	if !valid {
		return
	}
	var input struct{ Action, Destination, Password, Confirm string }
	if !decode(w, r, &input) {
		return
	}
	switch input.Action {
	case "mailbox_retire", "mailbox_restore", "mailbox_purge", "mailbox_rename":
	default:
		fail(w, http.StatusBadRequest, "请选择有效的邮箱管理操作")
		return
	}
	destination := strings.ToLower(strings.TrimSpace(input.Destination))
	if input.Action == "mailbox_rename" {
		if canonical, err := address(destination); err != nil || canonical != destination ||
			destination == owner || !validDNSName(emailDomain(destination)) {
			fail(w, http.StatusBadRequest, "请输入不同的有效新邮箱地址")
			return
		}
	}
	if input.Action == "mailbox_purge" && input.Confirm != owner {
		fail(w, http.StatusBadRequest, "请输入完整邮箱地址确认清除")
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
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
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
	var hash []byte
	err = tx.QueryRowContext(ctx, `SELECT password_hash FROM managed_accounts WHERE email=?`, owner).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "仅支持管理在后台创建的邮箱")
		return
	}
	if letterError(w, err) {
		return
	}
	var previouslySuspended bool
	err = tx.QueryRowContext(ctx, `SELECT previous_suspended FROM mailbox_retirements WHERE owner=?`, owner).Scan(&previouslySuspended)
	retired := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		letterError(w, err)
		return
	}
	mustBeRetired := input.Action == "mailbox_restore" || input.Action == "mailbox_purge"
	if retired != mustBeRetired {
		fail(w, http.StatusConflict, "邮箱状态已变化，请刷新账号列表")
		return
	}
	if input.Action != "mailbox_restore" {
		pending, err := mailboxHasPendingDelivery(ctx, tx, owner)
		if letterError(w, err) {
			return
		}
		if pending {
			fail(w, http.StatusConflict, "该邮箱仍有待处理或待确认的投递，应先结案")
			return
		}
	}
	suspended, err := a.lockAccountAccess(ctx, tx, owner)
	if letterError(w, err) {
		return
	}
	now := time.Now().Unix()
	switch input.Action {
	case "mailbox_retire":
		_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_retirements(owner,retired_at,previous_suspended) VALUES(?,?,?)`,
			owner, now, suspended)
		if err == nil {
			err = suspendAndRevokeMailbox(ctx, tx, owner)
		}
	case "mailbox_restore":
		_, err = tx.ExecContext(ctx, `DELETE FROM mailbox_retirements WHERE owner=?`, owner)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE account_access SET suspended=? WHERE owner=?`, previouslySuspended, owner)
		}
	case "mailbox_purge":
		err = purgeRetiredMailbox(ctx, tx, owner)
	case "mailbox_rename":
		target, lookup := a.destination(ctx, tx, destination)
		if errors.Is(lookup, errDestinationChanged) || (lookup == nil && target.owner != "") {
			fail(w, http.StatusConflict, "新地址已有主邮箱、别名或历史保留")
			return
		}
		if letterError(w, lookup) {
			return
		}
		reserved, lookup := a.priorMailboxExists(ctx, tx, destination)
		if letterError(w, lookup) {
			return
		}
		if reserved {
			fail(w, http.StatusConflict, "新地址有历史数据，不能用于迁移")
			return
		}
		err = a.renameManagedMailbox(ctx, tx, owner, destination, hash, suspended)
	}
	if letterError(w, err) {
		return
	}
	if input.Action == "mailbox_rename" {
		_, err = tx.ExecContext(ctx, `INSERT INTO admin_alias_actions(actor,target,alias,action,stamp) VALUES(?,?,?,?,?)`,
			actor, destination, owner, "alias_add", now)
		if letterError(w, err) {
			return
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_account_actions(actor,target,action,affected,stamp) VALUES(?,?,?,?,?)`,
		actor, owner, input.Action, 0, now)
	if !letterError(w, err) && !letterError(w, a.commitMailbox(tx, owner, destination)) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func mailboxHasPendingDelivery(ctx context.Context, tx *sql.Tx, owner string) (bool, error) {
	var pending bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM outbox o LEFT JOIN outbox_schedule s ON s.outbox_id=o.id
		WHERE o.state<>'failed' AND (o.sender=? OR s.local_owner=? OR o.recipient=?))`, owner, owner, owner).Scan(&pending)
	if err != nil || pending {
		return pending, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.envelope FROM outbox o JOIN web_outbox_envelopes e ON e.outbox_id=o.id
		WHERE o.state<>'failed' AND instr(e.envelope,?)>0`, owner)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return false, err
		}
		envelope, err := parseWebEnvelope(value)
		if err != nil {
			return false, err
		}
		for _, target := range envelope.Targets {
			if target.Owner == owner {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}

func suspendAndRevokeMailbox(ctx context.Context, tx *sql.Tx, owner string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE account_access SET suspended=TRUE WHERE owner=?`, owner); err != nil {
		return err
	}
	for _, table := range []string{"sessions", "web_session_details", "app_passwords"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE owner=?", owner); err != nil {
			return err
		}
	}
	return nil
}

var mailboxOwnedSettings = []string{"contacts", "custom_folders", "mail_rules", "mail_labels", "mail_templates",
	"mailbox_preferences", "mailbox_send_delay", "mailbox_storage_alerts", "signature_settings", "saved_searches",
	"account_password_overrides", "draft_creation_keys", "mail_threads", "spam_feedback"}

func purgeRetiredMailbox(ctx context.Context, tx *sql.Tx, owner string) error {
	for _, table := range []string{"draft_creation_keys", "imap_deleted", "imap_uids", "web_drafts", "web_letter_recipients",
		"mail_threads", "mail_snoozes", "mail_label_links", "spam_feedback"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE letter_id IN (SELECT id FROM letters WHERE owner=?)", owner); err != nil {
			return err
		}
	}
	for _, table := range []string{"outbox_schedule", "web_outbox_envelopes"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE outbox_id IN (SELECT id FROM outbox WHERE sender=?)", owner); err != nil {
			return err
		}
	}
	for _, query := range []string{
		`DELETE FROM letters WHERE owner=?`, `DELETE FROM outbox WHERE sender=?`,
		`DELETE FROM outbox_recipient_progress WHERE sender=?`,
		`DELETE FROM managed_aliases WHERE owner=?`, `DELETE FROM mailbox_quota WHERE owner=?`,
		`DELETE FROM managed_accounts WHERE email=?`,
	} {
		if _, err := tx.ExecContext(ctx, query, owner); err != nil {
			return err
		}
	}
	for _, table := range mailboxOwnedSettings {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE owner=?", owner); err != nil {
			return err
		}
	}
	return suspendAndRevokeMailbox(ctx, tx, owner)
}

func (a *app) renameManagedMailbox(ctx context.Context, tx *sql.Tx, owner, destination string, hash []byte, suspended bool) error {
	if err := suspendAndRevokeMailbox(ctx, tx, owner); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO managed_accounts(email,password_hash) VALUES(?,?)`, destination, hash); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_access(owner,suspended) VALUES(?,?)`, destination, suspended); err != nil {
		return err
	}
	for _, table := range append(append([]string{}, mailboxOwnedSettings...), "letters", "managed_aliases") {
		if _, err := tx.ExecContext(ctx, "UPDATE "+table+" SET owner=? WHERE owner=?", destination, owner); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE outbox SET sender=? WHERE sender=?`, destination, owner); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE outbox_recipient_progress SET sender=? WHERE sender=?`, destination, owner); err != nil {
		return err
	}
	if a.hardQuota {
		var limit int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT limit_mib FROM mailbox_quota WHERE owner=?),0)`, owner).Scan(&limit); err != nil {
			return err
		}
		upsert := " ON CONFLICT(owner) DO UPDATE SET limit_mib=excluded.limit_mib"
		if a.dbDriver == "mysql" {
			upsert = " ON DUPLICATE KEY UPDATE limit_mib=VALUES(limit_mib)"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_quota(owner,used_bytes,limit_mib) VALUES(?,0,?)`+upsert, destination, limit); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM mailbox_quota WHERE owner=?`, owner); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM managed_accounts WHERE email=?`, owner); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO managed_aliases(alias,owner) VALUES(?,?)`, owner, destination)
	return err
}
