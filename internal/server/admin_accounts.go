package server

import (
	"context"
	"database/sql"
	"net/http"
	"time"
)

type adminAccountAction struct {
	Actor    string `json:"actor"`
	Action   string `json:"action"`
	Affected int64  `json:"affected"`
	Stamp    int64  `json:"stamp"`
}

func (a *app) adminAccountList(w http.ResponseWriter, r *http.Request, actor string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	suspended, err := a.suspendedAccounts(ctx)
	if letterError(w, err) {
		return
	}
	retired, err := a.retiredAccounts(ctx)
	if letterError(w, err) {
		return
	}
	names, err := a.allAccountNames(ctx)
	if letterError(w, err) {
		return
	}
	aliases, err := a.effectiveAliases(ctx, names)
	if letterError(w, err) {
		return
	}
	admins, err := a.adminRoleSet(ctx, a.db)
	if letterError(w, err) {
		return
	}
	if !admins[actor] {
		for name := range names {
			if admins[name] || !a.domainAdmins[actor][emailDomain(name)] {
				delete(names, name)
			}
		}
		for alias, owner := range aliases {
			if !a.canManageDomainAlias(actor, alias, owner, admins) {
				delete(aliases, alias)
			}
		}
	}
	domains, err := a.adminDomains(ctx, names, aliases)
	if letterError(w, err) {
		return
	}
	reply(w, http.StatusOK, map[string]any{
		"email": actor, "accounts": a.adminAccounts(names, suspended, retired, admins, aliases), "domains": a.scopedDomains(actor, domains, admins[actor]),
	})
}

func (a *app) adminAccountAccess(w http.ResponseWriter, r *http.Request, actor string) {
	target := r.PathValue("email")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	credential, err := a.baseAccountDigest(ctx, a.db, target)
	if letterError(w, err) {
		return
	}
	now := time.Now().Unix()
	webCredential := a.webSessionCredential(target, credential)
	var sessions, passwords int64
	var suspended bool
	err = a.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM sessions WHERE owner=? AND credential=? AND expires>?),
		(SELECT COUNT(*) FROM app_passwords WHERE owner=? AND credential=? AND (expires=0 OR expires>?)),
		EXISTS(SELECT 1 FROM account_access WHERE owner=? AND suspended=TRUE)`,
		target, webCredential[:], now, target, credential[:], now, target).Scan(&sessions, &passwords, &suspended)
	if letterError(w, err) {
		return
	}
	administrator, err := a.isGlobalAdmin(ctx, a.db, target)
	if letterError(w, err) {
		return
	}
	canManageRole, err := a.canManageAdminRoles(ctx, a.db, actor, target)
	if letterError(w, err) {
		return
	}
	rows, err := a.db.QueryContext(ctx, `SELECT actor,action,affected,stamp FROM admin_account_actions
		WHERE target=? ORDER BY id DESC LIMIT 20`, target)
	if letterError(w, err) {
		return
	}
	defer rows.Close()
	audit := make([]adminAccountAction, 0, 20)
	for rows.Next() {
		var item adminAccountAction
		if letterError(w, rows.Scan(&item.Actor, &item.Action, &item.Affected, &item.Stamp)) {
			return
		}
		audit = append(audit, item)
	}
	if !letterError(w, rows.Err()) {
		reply(w, http.StatusOK, map[string]any{
			"sessions": sessions, "appPasswords": passwords, "audit": audit,
			"suspended": suspended, "canSuspend": !administrator,
			"administrator": administrator, "canManageRole": canManageRole,
		})
	}
}

func (a *app) adminChangeAccountAccess(w http.ResponseWriter, r *http.Request, actor string) {
	target := r.PathValue("email")
	if _, err := a.baseAccountHash(r.Context(), a.db, target); letterError(w, err) {
		return
	}
	var input struct {
		Scope    string
		Password string
	}
	if !decode(w, r, &input) {
		return
	}
	switch input.Scope {
	case "sessions", "app_passwords", "suspend", "restore":
	default:
		fail(w, http.StatusBadRequest, "请选择账号访问操作")
		return
	}
	if _, err := a.authenticate(actor, input.Password); err != nil {
		if !loginTemporaryError(w, err) {
			fail(w, http.StatusForbidden, "管理员主邮箱密码不正确")
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
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
	var retired bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mailbox_retirements WHERE owner=?)`, target).Scan(&retired)
	if letterError(w, err) {
		return
	}
	if retired {
		fail(w, http.StatusConflict, "邮箱已回收，请先通过邮箱管理恢复")
		return
	}
	if input.Scope == "suspend" {
		administrator, err := a.isGlobalAdmin(ctx, tx, target)
		if letterError(w, err) {
			return
		}
		if administrator {
			fail(w, http.StatusForbidden, "请先撤销管理员权限，再停用账号")
			return
		}
	}
	if input.Scope == "suspend" || input.Scope == "restore" {
		suspended, err := a.lockAccountAccess(ctx, tx, target)
		if letterError(w, err) {
			return
		}
		next := input.Scope == "suspend"
		if suspended == next {
			fail(w, http.StatusConflict, "账号状态已变化，请刷新后重试")
			return
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_access SET suspended=? WHERE owner=?`, next, target); letterError(w, err) {
			return
		}
	}
	affected, err := a.revokeAccountCredentials(ctx, tx, target, input.Scope)
	if letterError(w, err) {
		return
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_account_actions(actor,target,action,affected,stamp)
		VALUES(?,?,?,?,?)`, actor, target, input.Scope, affected, time.Now().Unix()); letterError(w, err) {
		return
	}
	if letterError(w, a.commitMailbox(tx, target)) {
		return
	}
	signedOut := actor == target && input.Scope == "sessions"
	if signedOut {
		a.cookie(w, "", -1)
	}
	reply(w, http.StatusOK, map[string]any{"affected": affected, "signedOut": signedOut})
}

func (a *app) revokeAccountCredentials(ctx context.Context, tx *sql.Tx, target, scope string) (int64, error) {
	var affected int64
	switch scope {
	case "sessions", "suspend", "password_reset":
		if _, err := a.lockAccountAccess(ctx, tx, target); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_access SET protocol_generation=protocol_generation+1 WHERE owner=?`, target); err != nil {
			return 0, err
		}
	}
	if scope == "sessions" || scope == "suspend" || scope == "password_reset" {
		result, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE owner=?`, target)
		if err != nil {
			return 0, err
		}
		affected, err = result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM web_session_details WHERE owner=?`, target); err != nil {
			return 0, err
		}
	}
	if scope == "app_passwords" || scope == "suspend" || scope == "password_reset" {
		result, err := tx.ExecContext(ctx, `DELETE FROM app_passwords WHERE owner=?`, target)
		if err != nil {
			return 0, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		affected += count
	}
	return affected, nil
}
