package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"
)

type webSession struct {
	owner      string
	digest     []byte
	lastActive int64
}

func browserSessionDigest(r *http.Request) []byte {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return nil
	}
	digest := sha256.Sum256([]byte(cookie.Value))
	return digest[:]
}

func (a *app) browserSession(r *http.Request) (webSession, error) {
	digest := browserSessionDigest(r)
	if digest == nil {
		return webSession{}, nil
	}
	result := webSession{digest: digest}
	var credential, managedHash []byte
	err := a.db.QueryRowContext(r.Context(), `SELECT s.owner,s.credential,coalesce(d.last_active,0),m.password_hash
		FROM sessions s LEFT JOIN web_session_details d ON d.digest=s.digest
		LEFT JOIN managed_accounts m ON m.email=s.owner
		WHERE s.digest=? AND s.expires>? AND NOT EXISTS
			(SELECT 1 FROM account_access aa WHERE aa.owner=s.owner AND aa.suspended=TRUE)`,
		digest, time.Now().Unix()).Scan(&result.owner, &credential, &result.lastActive, &managedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return webSession{}, nil
	}
	if err != nil {
		return webSession{}, err
	}
	hash, ok := a.accounts[result.owner]
	if !ok && len(managedHash) > 0 {
		hash, ok = managedHash, true
	}
	expected := a.webSessionCredential(result.owner, sha256.Sum256(hash))
	if !ok || !bytes.Equal(credential, expected[:]) {
		return webSession{}, nil
	}
	return result, nil
}

func (a *app) touchBrowserSession(r *http.Request, session webSession) {
	now := time.Now().Unix()
	const interval = 60
	if session.lastActive == 0 || session.lastActive > now-interval || r.URL.Path == "/api/events" {
		return
	}
	// Activity is informational and is updated at most once per minute per session.
	_, _ = a.db.ExecContext(r.Context(), `UPDATE web_session_details SET last_active=? WHERE digest=? AND last_active<=?`,
		now, session.digest, now-interval)
}

func browserDevice(agent string) string {
	browser := "浏览器"
	switch {
	case strings.Contains(agent, "Edg/"), strings.Contains(agent, "EdgA/"), strings.Contains(agent, "EdgiOS/"):
		browser = "Edge"
	case strings.Contains(agent, "OPR/"), strings.Contains(agent, "Opera/"):
		browser = "Opera"
	case strings.Contains(agent, "Firefox/"), strings.Contains(agent, "FxiOS/"):
		browser = "Firefox"
	case strings.Contains(agent, "Chrome/"), strings.Contains(agent, "CriOS/"):
		browser = "Chrome"
	case strings.Contains(agent, "Safari/"):
		browser = "Safari"
	}
	platform := ""
	switch {
	case strings.Contains(agent, "Android"):
		platform = "Android"
	case strings.Contains(agent, "iPhone"):
		platform = "iPhone"
	case strings.Contains(agent, "iPad"):
		platform = "iPad"
	case strings.Contains(agent, "Windows"):
		platform = "Windows"
	case strings.Contains(agent, "CrOS"):
		platform = "ChromeOS"
	case strings.Contains(agent, "Macintosh"):
		platform = "macOS"
	case strings.Contains(agent, "Linux"):
		platform = "Linux"
	}
	if platform == "" {
		return browser
	}
	return browser + " · " + platform
}

func (a *app) createBrowserSession(r *http.Request, owner string, digest, verified []byte, mfaCode string) error {
	ctx := r.Context()
	now := time.Now()
	// Unrelated expired sessions can be cleaned independently of issuing this session.
	_, _ = a.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires<=?`, now.Unix())
	_, _ = a.db.ExecContext(ctx, `DELETE FROM web_session_details WHERE expires<=?`, now.Unix())
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := a.requireActiveAccount(ctx, tx, owner); err != nil {
		return err
	}
	if err := a.checkPrimaryPassword(ctx, tx, owner, verified); err != nil {
		return err
	}
	if err := a.consumeAdminMFA(ctx, tx, owner, mfaCode, now); err != nil {
		return err
	}
	expires := now.Add(sessionAge).Unix()
	prior := browserSessionDigest(r)
	if prior != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE digest=?`, prior); err != nil {
			return err
		}
	}
	credential, err := a.baseWebSessionCredential(ctx, tx, owner)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (digest,owner,expires,credential) VALUES(?,?,?,?)`,
		digest, owner, expires, credential[:]); err != nil {
		return err
	}
	if prior != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM web_session_details WHERE digest=?`, prior); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO web_session_details (digest,owner,device,created,last_active,expires) VALUES(?,?,?,?,?,?)`,
		digest, owner, browserDevice(r.UserAgent()), now.Unix(), now.Unix(), expires); err != nil {
		return err
	}
	return tx.Commit()
}

func webSessionID(digest []byte) string {
	id := sha256.Sum256(append([]byte("Harbor Mail browser session ID\x00"), digest...))
	return base64.RawURLEncoding.EncodeToString(id[:])
}

type browserSessionInfo struct {
	ID         string `json:"id"`
	Device     string `json:"device"`
	Created    int64  `json:"created"`
	LastActive int64  `json:"lastActive"`
	Expires    int64  `json:"expires"`
	Current    bool   `json:"current"`
}

func (a *app) listBrowserSessions(w http.ResponseWriter, r *http.Request, owner string) {
	current := browserSessionDigest(r)
	credential, err := a.baseWebSessionCredential(r.Context(), a.db, owner)
	if browserSessionError(w, err) {
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT s.digest,s.expires,coalesce(d.device,''),coalesce(d.last_active,0),
		coalesce(d.created,s.expires-?)
		FROM sessions s LEFT JOIN web_session_details d ON d.digest=s.digest
		WHERE s.owner=? AND s.credential=? AND s.expires>?
		ORDER BY CASE WHEN s.digest=? THEN 0 ELSE 1 END,s.expires DESC,s.digest`,
		int64(sessionAge/time.Second), owner, credential[:], time.Now().Unix(), current)
	if browserSessionError(w, err) {
		return
	}
	defer rows.Close()
	result := make([]browserSessionInfo, 0)
	for rows.Next() {
		var item browserSessionInfo
		var digest []byte
		if err := rows.Scan(&digest, &item.Expires, &item.Device, &item.LastActive, &item.Created); browserSessionError(w, err) {
			return
		}
		item.ID = webSessionID(digest)
		item.Current = bytes.Equal(current, digest)
		result = append(result, item)
	}
	if !browserSessionError(w, rows.Err()) {
		reply(w, http.StatusOK, result)
	}
}

func (a *app) resolveBrowserSession(r *http.Request, owner string) ([]byte, error) {
	id := r.PathValue("id")
	if len(id) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return nil, nil
	}
	credential, err := a.baseWebSessionCredential(r.Context(), a.db, owner)
	if err != nil {
		return nil, err
	}
	rows, err := a.db.QueryContext(r.Context(), `SELECT digest FROM sessions WHERE owner=? AND credential=? AND expires>? AND digest<>?`,
		owner, credential[:], time.Now().Unix(), browserSessionDigest(r))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var digest []byte
		if err := rows.Scan(&digest); err != nil {
			return nil, err
		}
		if webSessionID(digest) == id {
			return digest, nil
		}
	}
	return nil, rows.Err()
}

func browserSessionError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errBrowserSessionEnded) {
		fail(w, http.StatusUnauthorized, "请登录邮箱")
	} else {
		fail(w, http.StatusInternalServerError, "无法管理登录设备")
	}
	return true
}

var errBrowserSessionEnded = errors.New("browser session has ended")

func (a *app) lockBrowserSession(ctx context.Context, tx *sql.Tx, owner string, digest []byte) error {
	credential, err := a.baseWebSessionCredential(ctx, tx, owner)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE sessions SET expires=expires
		WHERE digest=? AND owner=? AND credential=? AND expires>?`,
		digest, owner, credential[:], time.Now().Unix())
	err = requireAffectedRow(result, err)
	if errors.Is(err, sql.ErrNoRows) {
		return errBrowserSessionEnded
	}
	return err
}

func (a *app) deleteBrowserSessions(r *http.Request, owner string, target []byte) error {
	ctx := r.Context()
	current := browserSessionDigest(r)
	predicate := ` WHERE owner=? AND digest=?`
	if target == nil {
		predicate = ` WHERE owner=? AND digest<>?`
		target = current
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Keep the calling session valid while applying the revocation.
	if err := a.lockBrowserSession(ctx, tx, owner, current); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions`+predicate, owner, target); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM web_session_details`+predicate, owner, target); err != nil {
		return err
	}
	return a.commitMailbox(tx, owner)
}

func (a *app) revokeBrowserSession(w http.ResponseWriter, r *http.Request, owner string) {
	digest, err := a.resolveBrowserSession(r, owner)
	if browserSessionError(w, err) {
		return
	}
	if digest != nil && browserSessionError(w, a.deleteBrowserSessions(r, owner, digest)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) revokeOtherBrowserSessions(w http.ResponseWriter, r *http.Request, owner string) {
	if !browserSessionError(w, a.deleteBrowserSessions(r, owner, nil)) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *app) logout(w http.ResponseWriter, r *http.Request, owner string) {
	if !browserSessionError(w, a.deleteBrowserSessions(r, owner, browserSessionDigest(r))) {
		a.cookie(w, "", -1)
		w.WriteHeader(http.StatusNoContent)
	}
}
