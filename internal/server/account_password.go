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
	if a.admins[target] {
		fail(w, http.StatusForbidden, "请通过服务器配置修改管理员密码")
		return
	}
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
	if _, err := a.authenticate(actor, input.Password); err != nil {
		if errors.Is(err, errLoginBusy) {
			fail(w, http.StatusTooManyRequests, "验证请求过多，请稍后重试")
		} else {
			fail(w, http.StatusForbidden, "管理员主邮箱密码不正确")
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	previous, err := a.primaryPasswordHash(ctx, a.db, target)
	if letterError(w, err) {
		return
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
	if browserSessionError(w, a.lockBrowserSession(ctx, tx, actor, browserSessionDigest(r))) {
		return
	}
	if _, err := a.lockAccountAccess(ctx, tx, target); letterError(w, err) {
		return
	}
	err = a.checkPrimaryPassword(ctx, tx, target, previous)
	if errors.Is(err, errPrimaryPasswordChanged) {
		fail(w, http.StatusConflict, "密码已被其他操作更新，请刷新后重试")
		return
	}
	if letterError(w, err) || letterError(w, a.storePrimaryPassword(ctx, tx, target, hash)) {
		return
	}
	affected, err := revokeAccountCredentials(ctx, tx, target, "password_reset")
	if letterError(w, err) {
		return
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_account_actions(actor,target,action,affected,stamp)
		VALUES(?,?,?,?,?)`, actor, target, "password_reset", affected, time.Now().Unix()); letterError(w, err) {
		return
	}
	if !letterError(w, a.commitMailbox(tx, target)) {
		reply(w, http.StatusOK, map[string]int64{"affected": affected})
	}
}
