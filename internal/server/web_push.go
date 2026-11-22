package server

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

const webPushTTL = 30 * time.Minute

type webPushService struct {
	privateKey string
	publicKey  string
	client     *http.Client
}

func (a *app) loadWebPushKeys(ctx context.Context) (*webPushService, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return nil, err
	}
	keys := &webPushService{client: &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
	err = tx.QueryRowContext(ctx, `SELECT private_key,public_key FROM web_push_identity WHERE id=1`).
		Scan(&keys.privateKey, &keys.publicKey)
	if errors.Is(err, sql.ErrNoRows) {
		keys.privateKey, keys.publicKey, err = webpush.GenerateVAPIDKeys()
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO web_push_identity(id,private_key,public_key) VALUES(1,?,?)`,
				keys.privateKey, keys.publicKey)
		}
	}
	if err != nil {
		return nil, err
	}
	return keys, tx.Commit()
}

func validWebPushEndpoint(endpoint string) bool {
	if len(endpoint) > 2048 {
		return false
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" ||
		parsed.Port() != "" || parsed.Path == "" || parsed.Path == "/" {
		return false
	}
	switch host := parsed.Host; host {
	case "fcm.googleapis.com":
		return strings.HasPrefix(parsed.Path, "/fcm/send/")
	case "updates.push.services.mozilla.com":
		return strings.HasPrefix(parsed.Path, "/wpush/")
	default:
		return strings.HasSuffix(host, ".push.apple.com") || strings.HasSuffix(host, ".notify.windows.com")
	}
}

func validWebPushSubscription(sub webpush.Subscription) bool {
	if !validWebPushEndpoint(sub.Endpoint) || len(sub.Keys.P256dh) != 87 || len(sub.Keys.Auth) != 22 {
		return false
	}
	public, err := base64.RawURLEncoding.Strict().DecodeString(sub.Keys.P256dh)
	if err != nil {
		return false
	}
	if _, err := ecdh.P256().NewPublicKey(public); err != nil {
		return false
	}
	auth, err := base64.RawURLEncoding.Strict().DecodeString(sub.Keys.Auth)
	return err == nil && len(auth) == 16
}

func (a *app) webPushStatus(w http.ResponseWriter, r *http.Request, owner string) {
	w.Header().Set("Cache-Control", "no-store")
	if a.webPush == nil {
		reply(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	var enabled bool
	if letterError(w, a.db.QueryRowContext(r.Context(), `SELECT EXISTS(
		SELECT 1 FROM web_push_subscriptions WHERE owner=? AND session_digest=?)`,
		owner, browserSessionDigest(r)).Scan(&enabled)) {
		return
	}
	reply(w, http.StatusOK, map[string]any{"available": true, "enabled": enabled, "publicKey": a.webPush.publicKey})
}

func (a *app) updateWebPush(w http.ResponseWriter, r *http.Request, owner string) {
	if a.webPush == nil {
		fail(w, http.StatusServiceUnavailable, "新邮件通知暂不可用")
		return
	}
	var sub webpush.Subscription
	if !decode(w, r, &sub) {
		return
	}
	if !validWebPushSubscription(sub) {
		fail(w, http.StatusBadRequest, "此浏览器的推送订阅无效或暂不支持")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if letterError(w, err) {
		return
	}
	defer tx.Rollback()
	session := browserSessionDigest(r)
	if letterError(w, lockIMAPUIDs(ctx, tx)) || browserSessionError(w, a.lockBrowserSession(ctx, tx, owner, session)) {
		return
	}
	id := sha256.Sum256([]byte(sub.Endpoint))
	// The same browser may opt in again after signing in with a fresh session.
	var conflict bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM web_push_subscriptions
		WHERE id=? AND (p256dh<>? OR auth_key<>?))`, id[:], sub.Keys.P256dh, sub.Keys.Auth).Scan(&conflict)
	if letterError(w, err) {
		return
	}
	if conflict {
		fail(w, http.StatusConflict, "订阅密钥已变化，请在浏览器中重新启用通知")
		return
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM web_push_subscriptions WHERE id=? OR (owner=? AND session_digest=?)`,
		id[:], owner, session)
	if letterError(w, err) {
		return
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO web_push_subscriptions(id,owner,session_digest,endpoint,p256dh,auth_key)
		VALUES(?,?,?,?,?,?)`, id[:], owner, session, sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth)
	if !letterError(w, err) && !letterError(w, tx.Commit()) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *app) disableWebPush(w http.ResponseWriter, r *http.Request, owner string) {
	_, err := a.db.ExecContext(r.Context(), `DELETE FROM web_push_subscriptions WHERE owner=? AND session_digest=?`,
		owner, browserSessionDigest(r))
	if !letterError(w, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func queueWebPush(ctx context.Context, tx *sql.Tx, owner string, id int64) error {
	now := time.Now().Unix()
	// One pending notification per browser. A new arrival replaces older pending work.
	_, err := tx.ExecContext(ctx, `UPDATE web_push_subscriptions SET pending_id=?, queued_at=?, due_at=?
		WHERE owner=? AND session_digest IN (SELECT digest FROM sessions WHERE owner=? AND expires>?)`,
		id, now, now, owner, owner, now)
	return err
}

type webPushJob struct {
	id, session, credential []byte
	owner                   string
	message, due, queued    int64
	sub                     webpush.Subscription
}

func (a *app) runWebPush(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var cleanAfter time.Time
	for {
		now := time.Now()
		if now.After(cleanAfter) {
			_, _ = a.db.ExecContext(ctx, `DELETE FROM web_push_subscriptions
				WHERE NOT EXISTS (SELECT 1 FROM sessions WHERE digest=session_digest AND expires>?)`, now.Unix())
			cleanAfter = now.Add(time.Hour)
		}
		for range 8 {
			if ctx.Err() != nil || !a.deliverWebPush(ctx) {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *app) deliverWebPush(ctx context.Context) bool {
	now := time.Now().Unix()
	j := webPushJob{}
	err := a.db.QueryRowContext(ctx, `SELECT p.id,p.owner,p.session_digest,s.credential,p.endpoint,p.p256dh,p.auth_key,
		p.pending_id,p.due_at,p.queued_at
		FROM web_push_subscriptions p JOIN sessions s ON s.digest=p.session_digest AND s.owner=p.owner
		WHERE p.due_at>0 AND p.due_at<=? AND s.expires>?
		AND NOT EXISTS (SELECT 1 FROM account_access WHERE owner=p.owner AND suspended=TRUE)
		ORDER BY p.due_at LIMIT 1`, now, now).
		Scan(&j.id, &j.owner, &j.session, &j.credential, &j.sub.Endpoint, &j.sub.Keys.P256dh, &j.sub.Keys.Auth,
			&j.message, &j.due, &j.queued)
	if err != nil {
		return false
	}
	claim, err := a.db.ExecContext(ctx, `UPDATE web_push_subscriptions SET due_at=?
		WHERE id=? AND session_digest=? AND pending_id=? AND due_at=?`, now+90, j.id, j.session, j.message, j.due)
	if requireAffectedRow(claim, err) != nil {
		return false
	}
	credential, err := a.baseWebSessionCredential(ctx, a.db, j.owner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return true // The lease expires and the worker can retry after the database recovers.
	}
	if err != nil || !bytes.Equal(credential[:], j.credential) || !validWebPushSubscription(j.sub) {
		a.finishWebPush(ctx, j, true)
		return true
	}
	remaining := int(webPushTTL/time.Second) - int(now-j.queued)
	if remaining <= 0 {
		a.finishWebPush(ctx, j, false)
		return true
	}
	// Keep delivery independent of SMTP and never send message contents through push providers.
	resp, err := webpush.SendNotificationWithContext(ctx, []byte("{}"), &j.sub, &webpush.Options{
		HTTPClient: a.webPush.client, Subscriber: "https://" + a.domain,
		VAPIDPublicKey: a.webPush.publicKey, VAPIDPrivateKey: a.webPush.privateKey,
		TTL: remaining, Topic: "harbor-inbox",
	})
	if err == nil {
		resp.Body.Close()
		switch code := resp.StatusCode; {
		case code == http.StatusGone || code == http.StatusNotFound:
			a.finishWebPush(ctx, j, true)
		case code >= 200 && code < 300, code >= 300 && code < 500 && code != http.StatusTooManyRequests:
			a.finishWebPush(ctx, j, false)
		}
	}
	return true
}

func (a *app) finishWebPush(ctx context.Context, j webPushJob, expired bool) {
	if expired {
		_, _ = a.db.ExecContext(ctx, `DELETE FROM web_push_subscriptions WHERE id=? AND session_digest=?`, j.id, j.session)
		return
	}
	_, _ = a.db.ExecContext(ctx, `UPDATE web_push_subscriptions SET due_at=0,pending_id=0,queued_at=0
		WHERE id=? AND session_digest=? AND pending_id=?`, j.id, j.session, j.message)
}
