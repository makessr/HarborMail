package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"
)

func (a *app) issuePasswordRecoveryCode(w http.ResponseWriter, r *http.Request, owner string) {
	var input struct{ Password string }
	if !decode(w, r, &input) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	source := loginSource(r, a.webLogin.proxies)
	if !a.checkWebLoginRate(w, ctx, source, owner, true) {
		return
	}
	_, verified, err := a.authenticatePrimary(ctx, owner, input.Password)
	if err != nil {
		if !loginTemporaryError(w, err) {
			fail(w, http.StatusForbidden, "当前主邮箱密码不正确")
		}
		return
	}
	code := token()
	digest := recoveryCodeDigest(code)
	tx, err := a.db.BeginTx(ctx, nil)
	if letterError(w, err) {
		return
	}
	defer tx.Rollback()
	if letterError(w, lockIMAPUIDs(ctx, tx)) ||
		browserSessionError(w, a.lockBrowserSession(ctx, tx, owner, browserSessionDigest(r))) {
		return
	}
	if err := a.checkPrimaryPassword(ctx, tx, owner, verified); errors.Is(err, errPrimaryPasswordChanged) {
		fail(w, http.StatusConflict, "密码已更新，请重新登录")
		return
	} else if letterError(w, err) {
		return
	}
	base, err := a.baseAccountDigest(ctx, tx, owner)
	if letterError(w, err) {
		return
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM password_recovery_codes WHERE owner=?", owner); letterError(w, err) {
		return
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO password_recovery_codes(owner,base_digest,code_digest) VALUES(?,?,?)",
		owner, base[:], digest[:])
	if !letterError(w, err) && !letterError(w, tx.Commit()) {
		reply(w, http.StatusCreated, map[string]string{"code": code})
	}
}

func recoveryCodeDigest(code string) [sha256.Size]byte {
	return sha256.Sum256([]byte("Harbor Mail password recovery\x00" + code))
}

func (a *app) verifiedRecoveryCode(ctx context.Context, db folderQueryer, owner string, digest [sha256.Size]byte) (bool, error) {
	base, err := a.baseAccountDigest(ctx, db, owner)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var valid bool
	err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM password_recovery_codes
		WHERE owner=? AND base_digest=? AND code_digest=?
		AND NOT EXISTS(SELECT 1 FROM account_access WHERE owner=? AND suspended=TRUE))`,
		owner, base[:], digest[:], owner).Scan(&valid)
	return valid, err
}

func (a *app) recoverPrimaryPassword(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	source := loginSource(r, a.webLogin.proxies)
	if !a.checkWebLoginRate(w, ctx, source, "", false) {
		return
	}
	var input struct{ Email, Code, NewPassword string }
	if !decode(w, r, &input) {
		return
	}
	if !validNewPrimaryPassword(input.NewPassword) {
		fail(w, http.StatusBadRequest, "新密码长度须为 12 至 72 字节，且不能含换行或空字符")
		return
	}
	reject := func() { fail(w, http.StatusForbidden, "邮箱或密码重置码无效") }
	owner := strings.ToLower(strings.TrimSpace(input.Email))
	code := strings.ToLower(strings.TrimSpace(input.Code))
	if len(owner) == 0 || len(owner) > 254 || len(code) != 64 {
		reject()
		return
	}
	if _, err := hex.DecodeString(code); err != nil {
		reject()
		return
	}
	digest := recoveryCodeDigest(code)
	valid, err := a.verifiedRecoveryCode(ctx, a.db, owner, digest)
	if letterError(w, err) {
		return
	}
	if !valid {
		reject()
		return
	}
	previous, err := a.primaryPasswordHash(ctx, a.db, owner)
	if errors.Is(err, sql.ErrNoRows) {
		reject()
		return
	}
	if letterError(w, err) {
		return
	}
	hash, err := hashPrimaryPassword(previous, input.NewPassword)
	if loginTemporaryError(w, err) {
		return
	}
	if errors.Is(err, errPrimaryPasswordReused) {
		fail(w, http.StatusBadRequest, "新密码须与当前密码不同")
		return
	}
	if letterError(w, err) {
		return
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if letterError(w, err) {
		return
	}
	defer tx.Rollback()
	if letterError(w, lockIMAPUIDs(ctx, tx)) {
		return
	}
	valid, err = a.verifiedRecoveryCode(ctx, tx, owner, digest)
	if letterError(w, err) {
		return
	}
	if !valid {
		reject()
		return
	}
	_, err = a.replacePrimaryPassword(ctx, tx, owner, owner, "password_recovery", hash)
	if !letterError(w, err) && !letterError(w, a.commitMailbox(tx, owner)) {
		w.WriteHeader(http.StatusNoContent)
	}
}
