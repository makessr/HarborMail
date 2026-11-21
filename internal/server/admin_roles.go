package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net/http"
	"time"
)

func (a *app) isGlobalAdmin(ctx context.Context, db folderQueryer, owner string) (bool, error) {
	if a.admins[owner] {
		var retired bool
		err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM mailbox_retirements WHERE owner=?)", owner).Scan(&retired)
		return !retired && err == nil, err
	}
	var granted []byte
	err := db.QueryRowContext(ctx, "SELECT base_digest FROM managed_admin_roles WHERE owner=?", owner).Scan(&granted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	current, err := a.baseAccountDigest(ctx, db, owner)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return bytes.Equal(granted, current[:]), err
}

type adminRoleReader interface {
	folderQueryer
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (a *app) adminRoleSet(ctx context.Context, db adminRoleReader) (map[string]bool, error) {
	admins := make(map[string]bool, len(a.admins))
	for owner := range a.admins {
		active, err := a.isGlobalAdmin(ctx, db, owner)
		if err != nil {
			return nil, err
		}
		if active {
			admins[owner] = true
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT r.owner,r.base_digest,m.password_hash
		FROM managed_admin_roles r LEFT JOIN managed_accounts m ON m.email=r.owner
		WHERE NOT EXISTS (SELECT 1 FROM mailbox_retirements x WHERE x.owner=r.owner)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var owner string
		var granted, managedHash []byte
		if err := rows.Scan(&owner, &granted, &managedHash); err != nil {
			return nil, err
		}
		if hash, exists := a.accounts[owner]; exists {
			managedHash = hash
		}
		digest := sha256.Sum256(managedHash)
		if len(managedHash) != 0 && bytes.Equal(granted, digest[:]) {
			admins[owner] = true
		}
	}
	return admins, rows.Err()
}

func (a *app) hasSignedInSuccessor(ctx context.Context, db adminRoleReader, outgoing string) (bool, error) {
	admins, err := a.adminRoleSet(ctx, db)
	if err != nil {
		return false, err
	}
	for owner := range admins {
		if owner == outgoing {
			continue
		}
		base, err := a.baseAccountDigest(ctx, db, owner)
		if err != nil {
			return false, err
		}
		credential := a.webSessionCredential(owner, base)
		var signedIn bool
		err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions s
			WHERE s.owner=? AND s.credential=? AND s.expires>? AND NOT EXISTS
				(SELECT 1 FROM account_access x WHERE x.owner=s.owner AND x.suspended=TRUE))`,
			owner, credential[:], time.Now().Unix()).Scan(&signedIn)
		if err != nil || signedIn {
			return signedIn, err
		}
	}
	return false, nil
}

func (a *app) adminChangeRole(w http.ResponseWriter, r *http.Request, actor string) {
	target := r.PathValue("email")
	var input struct {
		Admin    *bool
		Password string
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Admin == nil {
		fail(w, http.StatusBadRequest, "请选择要授予或撤销的权限")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	source := loginSource(r, a.webLogin.proxies)
	if retry := a.webLogin.reserve(ctx, source, actor, true); retry > 0 {
		a.rejectWebLoginRate(w, source, actor, retry)
		return
	}
	_, verified, err := a.authenticatePrimary(ctx, actor, input.Password)
	if errors.Is(err, errLoginBusy) {
		fail(w, http.StatusTooManyRequests, "验证请求过多，请稍后重试")
		return
	}
	if err != nil {
		fail(w, http.StatusForbidden, "管理员主邮箱密码不正确")
		return
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if letterError(w, err) {
		return
	}
	defer tx.Rollback()
	if letterError(w, lockIMAPUIDs(ctx, tx)) ||
		browserSessionError(w, a.lockBrowserSession(ctx, tx, actor, browserSessionDigest(r))) ||
		letterError(w, a.checkPrimaryPassword(ctx, tx, actor, verified)) {
		return
	}
	allowed, err := a.canManageAdminRoles(ctx, tx, actor, target)
	if letterError(w, err) {
		return
	}
	if !allowed {
		fail(w, http.StatusForbidden, "当前账号无权调整该管理员权限")
		return
	}
	digest, err := a.baseAccountDigest(ctx, tx, target)
	if letterError(w, err) {
		return
	}
	suspended, err := a.lockAccountAccess(ctx, tx, target)
	if letterError(w, err) {
		return
	}
	var retired bool
	if letterError(w, tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM mailbox_retirements WHERE owner=?)", target).Scan(&retired)) {
		return
	}
	if *input.Admin && (suspended || retired) {
		fail(w, http.StatusConflict, "仅可授权启用的主邮箱账号")
		return
	}
	current, err := a.isGlobalAdmin(ctx, tx, target)
	if letterError(w, err) {
		return
	}
	if current == *input.Admin {
		fail(w, http.StatusConflict, "账号权限已变化，请刷新后重试")
		return
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM managed_admin_roles WHERE owner=?", target); letterError(w, err) {
		return
	}
	action := "admin_revoke"
	if *input.Admin {
		action = "admin_grant"
		if _, err := tx.ExecContext(ctx, "INSERT INTO managed_admin_roles(owner,base_digest) VALUES(?,?)", target, digest[:]); letterError(w, err) {
			return
		}
	}
	affected, err := revokeAccountCredentials(ctx, tx, target, "sessions")
	if letterError(w, err) {
		return
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_account_actions(actor,target,action,affected,stamp)
		VALUES(?,?,?,?,?)`, actor, target, action, affected, time.Now().Unix()); letterError(w, err) {
		return
	}
	if !letterError(w, a.commitMailbox(tx, target)) {
		reply(w, http.StatusOK, map[string]any{"admin": *input.Admin, "affected": affected})
	}
}

func (a *app) canManageAdminRoles(ctx context.Context, db folderQueryer, actor, target string) (bool, error) {
	if actor == target || a.admins[target] {
		return false, nil
	}
	administrator, err := a.isGlobalAdmin(ctx, db, actor)
	if err != nil || !administrator || a.admins[actor] {
		return administrator, err
	}
	for owner := range a.admins {
		active, err := a.isGlobalAdmin(ctx, db, owner)
		if err != nil || active {
			return false, err
		}
	}
	return true, nil
}
