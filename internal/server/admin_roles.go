package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"maps"
	"net/http"
	"time"
)

func (a *app) isGlobalAdmin(ctx context.Context, db folderQueryer, owner string) (bool, error) {
	if a.admins[owner] {
		return true, nil
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

func (a *app) adminRoleSet(ctx context.Context) (map[string]bool, error) {
	admins := make(map[string]bool, len(a.admins))
	maps.Copy(admins, a.admins)
	rows, err := a.db.QueryContext(ctx, `SELECT r.owner,r.base_digest,m.password_hash
		FROM managed_admin_roles r LEFT JOIN managed_accounts m ON m.email=r.owner`)
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

func (a *app) adminChangeRole(w http.ResponseWriter, r *http.Request, actor string) {
	if !a.admins[actor] {
		fail(w, http.StatusForbidden, "仅服务器配置的管理员可以调整管理员权限")
		return
	}
	target := r.PathValue("email")
	if a.admins[target] {
		fail(w, http.StatusForbidden, "服务器配置的管理员权限由部署配置维护")
		return
	}
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
