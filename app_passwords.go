package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type mailProtocol int

const (
	clientIMAP mailProtocol = 1 << iota
	clientPOP3
	clientSMTP
)

const appPasswordPrefix = "hmapp_"

var protocolNames = []struct {
	name string
	bit  mailProtocol
}{
	{"imap", clientIMAP},
	{"pop3", clientPOP3},
	{"smtp", clientSMTP},
}

func scopeFromNames(names []string) mailProtocol {
	var scope mailProtocol
	for _, name := range names {
		var bit mailProtocol
		for _, protocol := range protocolNames {
			if protocol.name == name {
				bit = protocol.bit
				break
			}
		}
		if bit == 0 || scope&bit != 0 {
			return 0
		}
		scope |= bit
	}
	return scope
}

func (scope mailProtocol) names() []string {
	names := make([]string, 0, 3)
	for _, protocol := range protocolNames {
		if scope&protocol.bit != 0 {
			names = append(names, protocol.name)
		}
	}
	return names
}

func isAppPassword(password string) bool {
	payload, found := strings.CutPrefix(password, appPasswordPrefix)
	if !found || len(payload) != 64 {
		return false
	}
	_, err := hex.DecodeString(payload)
	return err == nil
}

func (a *app) authenticateClient(email, password string, protocol mailProtocol) (string, error) {
	owner := strings.ToLower(strings.TrimSpace(email))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !isAppPassword(password) {
		return a.authenticateClientPrimary(ctx, email, password)
	}
	credential, err := a.baseAccountDigest(ctx, a.db, owner)
	if errors.Is(err, sql.ErrNoRows) {
		return a.authenticateClientPrimary(ctx, email, password)
	}
	if err != nil {
		return "", errLoginBusy
	}
	digest := sha256.Sum256([]byte(password))
	now := time.Now().Unix()
	result, err := a.db.ExecContext(ctx, `UPDATE app_passwords SET last_used=?
		WHERE owner=? AND digest=? AND credential=? AND (protocols & ?)<>0 AND (expires=0 OR expires>?)
		AND NOT EXISTS (SELECT 1 FROM account_access WHERE owner=? AND suspended=TRUE)`,
		now, owner, digest[:], credential[:], protocol, now, owner)
	if err != nil {
		log.Print("client credential lookup: ", err)
		return "", errLoginBusy
	}
	count, err := result.RowsAffected()
	if err != nil {
		return "", errLoginBusy
	}
	if count == 1 {
		return owner, nil
	}
	// Keep accepting primary passwords even if they happen to use this format.
	return a.authenticateClientPrimary(ctx, email, password)
}

func (a *app) authenticateClientPrimary(ctx context.Context, email, password string) (string, error) {
	owner, verified, err := a.authenticatePrimary(ctx, email, password)
	if err != nil {
		return "", err
	}
	var suspended bool
	if err := a.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM account_access WHERE owner=? AND suspended=TRUE)`, owner).Scan(&suspended); err != nil {
		return "", errLoginBusy
	}
	if suspended {
		return "", errLoginInvalid
	}
	if err := a.checkPrimaryPassword(ctx, a.db, owner, verified); err != nil {
		if errors.Is(err, errPrimaryPasswordChanged) {
			return "", errLoginInvalid
		}
		return "", errLoginBusy
	}
	return owner, nil
}

type appPassword struct {
	ID        int64    `json:"id"`
	Label     string   `json:"label"`
	Protocols []string `json:"protocols"`
	Created   int64    `json:"created"`
	Expires   int64    `json:"expires"`
	LastUsed  int64    `json:"lastUsed"`
}

func (a *app) listAppPasswords(w http.ResponseWriter, r *http.Request, owner string) {
	credential, err := a.baseAccountDigest(r.Context(), a.db, owner)
	if appPasswordHTTPError(w, err) {
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,label,protocols,created,expires,last_used FROM app_passwords
		WHERE owner=? AND credential=? ORDER BY id DESC`, owner, credential[:])
	if appPasswordHTTPError(w, err) {
		return
	}
	defer rows.Close()
	result := make([]appPassword, 0)
	for rows.Next() {
		var item appPassword
		var scope mailProtocol
		err := rows.Scan(&item.ID, &item.Label, &scope, &item.Created, &item.Expires, &item.LastUsed)
		if appPasswordHTTPError(w, err) {
			return
		}
		item.Protocols = scope.names()
		result = append(result, item)
	}
	if !appPasswordHTTPError(w, rows.Err()) {
		reply(w, http.StatusOK, result)
	}
}

func appPasswordHTTPError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	log.Print("app password operation: ", err)
	fail(w, http.StatusInternalServerError, "无法管理应用密码")
	return true
}

func (a *app) createAppPassword(w http.ResponseWriter, r *http.Request, owner string) {
	var input struct {
		Label     string
		Password  string
		Days      int
		Protocols []string
	}
	if !decode(w, r, &input) {
		return
	}
	label := strings.TrimSpace(input.Label)
	scope := scopeFromNames(input.Protocols)
	if label == "" || utf8.RuneCountInString(label) > 80 || strings.ContainsFunc(label, unicode.IsControl) ||
		scope == 0 || input.Days < 0 || input.Days > 365 {
		fail(w, http.StatusBadRequest, "请填写名称、选择协议和有效期")
		return
	}
	_, verified, err := a.authenticatePrimary(r.Context(), owner, input.Password)
	if err != nil {
		if errors.Is(err, errLoginBusy) {
			fail(w, http.StatusTooManyRequests, "验证请求过多，请稍后重试")
		} else {
			fail(w, http.StatusForbidden, "主邮箱密码不正确")
		}
		return
	}
	now := time.Now()
	item := appPassword{Label: label, Protocols: scope.names(), Created: now.Unix()}
	if input.Days != 0 {
		item.Expires = now.Add(time.Duration(input.Days) * 24 * time.Hour).Unix()
	}
	password := appPasswordPrefix + token()
	digest := sha256.Sum256([]byte(password))
	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if appPasswordHTTPError(w, err) {
		return
	}
	defer tx.Rollback()
	if err := a.requireActiveAccount(ctx, tx, owner); err != nil {
		if errors.Is(err, errAccountSuspended) {
			fail(w, http.StatusUnauthorized, "请登录邮箱")
		} else {
			appPasswordHTTPError(w, err)
		}
		return
	}
	if browserSessionError(w, a.lockBrowserSession(ctx, tx, owner, browserSessionDigest(r))) {
		return
	}
	if err := a.checkPrimaryPassword(ctx, tx, owner, verified); err != nil {
		if errors.Is(err, errPrimaryPasswordChanged) {
			fail(w, http.StatusUnauthorized, "请使用当前密码重新登录")
		} else {
			appPasswordHTTPError(w, err)
		}
		return
	}
	credential, err := a.baseAccountDigest(ctx, tx, owner)
	if appPasswordHTTPError(w, err) {
		return
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO app_passwords
		(owner,digest,credential,label,protocols,created,expires) VALUES (?,?,?,?,?,?,?)`,
		owner, digest[:], credential[:], label, scope, item.Created, item.Expires)
	if appPasswordHTTPError(w, err) {
		return
	}
	item.ID, err = result.LastInsertId()
	if !appPasswordHTTPError(w, err) && !appPasswordHTTPError(w, tx.Commit()) {
		reply(w, http.StatusCreated, struct {
			appPassword
			Secret string `json:"secret"`
		}{item, password})
	}
}

func (a *app) revokeAppPassword(w http.ResponseWriter, r *http.Request, owner string) {
	credential, err := a.baseAccountDigest(r.Context(), a.db, owner)
	if appPasswordHTTPError(w, err) {
		return
	}
	_, err = a.db.ExecContext(r.Context(), `DELETE FROM app_passwords WHERE owner=? AND id=? AND credential=?`,
		owner, letterID(r), credential[:])
	if !appPasswordHTTPError(w, err) {
		// Repeated revocations and requests for unknown IDs have the same outcome.
		w.WriteHeader(http.StatusNoContent)
	}
}
