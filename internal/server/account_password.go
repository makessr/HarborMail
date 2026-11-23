package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var errPrimaryPasswordChanged = errors.New("primary password changed")
var errPrimaryPasswordReused = errors.New("new password equals current password")

func (a *app) primaryPasswordHash(ctx context.Context, db folderQueryer, owner string) ([]byte, error) {
	base, baseErr := a.baseAccountHash(ctx, db, owner)
	if baseErr != nil {
		return nil, baseErr
	}
	digest := sha256.Sum256(base)
	var override []byte
	err := db.QueryRowContext(ctx, `SELECT password_hash FROM account_password_overrides WHERE owner=? AND base_digest=?`,
		owner, digest[:]).Scan(&override)
	if errors.Is(err, sql.ErrNoRows) {
		return base, nil
	}
	return override, err
}

func (a *app) checkPrimaryPassword(ctx context.Context, db folderQueryer, owner string, verified []byte) error {
	current, err := a.primaryPasswordHash(ctx, db, owner)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, verified) {
		return errPrimaryPasswordChanged
	}
	return nil
}

func (a *app) storePrimaryPassword(ctx context.Context, tx *sql.Tx, owner string, hash []byte) error {
	digest, err := a.baseAccountDigest(ctx, tx, owner)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_password_overrides WHERE owner=?`, owner); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO account_password_overrides(owner,base_digest,password_hash) VALUES(?,?,?)`,
		owner, digest[:], hash)
	return err
}

func hashPrimaryPassword(previous []byte, password string) ([]byte, error) {
	select {
	case loginSlots <- struct{}{}:
		defer func() { <-loginSlots }()
	default:
		return nil, errLoginBusy
	}
	if len(previous) != 0 && bcrypt.CompareHashAndPassword(previous, []byte(password)) == nil {
		return nil, errPrimaryPasswordReused
	}
	return bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
}

func validNewPrimaryPassword(password string) bool {
	return len(password) >= 12 && len(password) <= 72 && !strings.ContainsAny(password, "\r\n\x00")
}

func (a *app) adminResetPassword(w http.ResponseWriter, r *http.Request, actor string) {
	target := r.PathValue("email")
	a.updatePrimaryPassword(w, r, actor, target, "password_reset")
}

func (a *app) changeOwnPassword(w http.ResponseWriter, r *http.Request, actor string) {
	a.updatePrimaryPassword(w, r, actor, actor, "password_change")
}

func (a *app) updatePrimaryPassword(w http.ResponseWriter, r *http.Request, actor, target, action string) {
	var input struct {
		Password    string
		NewPassword string
	}
	if !decode(w, r, &input) {
		return
	}
	if !validNewPrimaryPassword(input.NewPassword) {
		fail(w, http.StatusBadRequest, "新密码长度须为 12 至 72 字节，且不能含换行或空字符")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if action == "password_change" {
		source := loginSource(r, a.webLogin.proxies)
		if !a.checkWebLoginRate(w, ctx, source, actor, true) {
			return
		}
	}
	_, verified, err := a.authenticatePrimary(ctx, actor, input.Password)
	if err != nil {
		if !loginTemporaryError(w, err) {
			fail(w, http.StatusForbidden, "当前主邮箱密码不正确")
		}
		return
	}
	previous := verified
	if actor != target {
		previous, err = a.primaryPasswordHash(ctx, a.db, target)
		if letterError(w, err) {
			return
		}
	}
	hash, err := hashPrimaryPassword(previous, input.NewPassword)
	switch {
	case errors.Is(err, errLoginBusy):
		fail(w, http.StatusTooManyRequests, "验证请求过多，请稍后重试")
		return
	case errors.Is(err, errPrimaryPasswordReused):
		fail(w, http.StatusBadRequest, "新密码须与当前密码不同")
		return
	case letterError(w, err):
		return
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if letterError(w, err) {
		return
	}
	defer tx.Rollback()
	if letterError(w, lockIMAPUIDs(ctx, tx)) || browserSessionError(w, a.lockBrowserSession(ctx, tx, actor, browserSessionDigest(r))) {
		return
	}
	if _, err := a.lockAccountAccess(ctx, tx, target); letterError(w, err) {
		return
	}
	if action == "password_reset" {
		administrator, err := a.isGlobalAdmin(ctx, tx, target)
		if letterError(w, err) {
			return
		}
		if administrator {
			fail(w, http.StatusForbidden, "管理员可在后台页首修改自己的密码")
			return
		}
	}
	err = a.checkPrimaryPassword(ctx, tx, target, previous)
	if errors.Is(err, errPrimaryPasswordChanged) {
		fail(w, http.StatusConflict, "密码已被其他操作更新，请刷新后重试")
		return
	}
	if letterError(w, err) {
		return
	}
	affected, err := a.replacePrimaryPassword(ctx, tx, actor, target, action, hash)
	if letterError(w, err) || letterError(w, a.commitMailbox(tx, target)) {
		return
	}
	if actor == target {
		a.cookie(w, "", -1)
	}
	reply(w, http.StatusOK, map[string]any{"affected": affected, "signedOut": actor == target})
}

func (a *app) replacePrimaryPassword(ctx context.Context, tx *sql.Tx, actor, target, action string, hash []byte) (int64, error) {
	if err := a.storePrimaryPassword(ctx, tx, target, hash); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM password_recovery_codes WHERE owner=?`, target); err != nil {
		return 0, err
	}
	affected, err := a.revokeAccountCredentials(ctx, tx, target, "password_reset")
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_account_actions(actor,target,action,affected,stamp)
		VALUES(?,?,?,?,?)`, actor, target, action, affected, time.Now().Unix())
	return affected, err
}
