package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"
)

const mailboxMFASeedSize = 20
const mailboxMFASeedContext = "Harbor Mail mailbox TOTP seed"

type mailboxMFA struct {
	enabled        bool
	sealedSeed     []byte
	recoveryHashes string
	lastStep       int64
}

func readMailboxMFA(ctx context.Context, db folderQueryer, owner string) (mailboxMFA, error) {
	var m mailboxMFA
	err := db.QueryRowContext(ctx, `SELECT sealed_seed,recovery_hashes,last_step FROM mailbox_mfa WHERE owner=?`, owner).
		Scan(&m.sealedSeed, &m.recoveryHashes, &m.lastStep)
	m.enabled = err == nil
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return m, err
}

func totpStep(seed []byte, code string, unix int64) (int64, bool) {
	if len(code) != 6 {
		return 0, false
	}
	step := unix / 30
	for candidate := step + 1; candidate >= step-1; candidate-- {
		if subtle.ConstantTimeCompare([]byte(code), []byte(adminTOTP(seed, candidate))) == 1 {
			return candidate, true
		}
	}
	return 0, false
}

func (a *app) mfaUnix(ctx context.Context, tx *sql.Tx, now time.Time) (int64, error) {
	unix := now.Unix()
	if a.dbDriver == "mysql" {
		err := tx.QueryRowContext(ctx, "SELECT UNIX_TIMESTAMP()").Scan(&unix)
		return unix, err
	}
	return unix, nil
}

// Called with the account access row locked, in the transaction that issues the session.
func (a *app) consumeBrowserMFA(ctx context.Context, tx *sql.Tx, owner, code string, now time.Time) error {
	if _, managed := a.adminMFA[owner]; managed {
		return a.consumeAdminMFA(ctx, tx, owner, code, now)
	}
	entry, err := readMailboxMFA(ctx, tx, owner)
	if err != nil || !entry.enabled {
		return err
	}
	return a.consumeMailboxMFA(ctx, tx, owner, code, entry, now)
}

func (a *app) consumeMailboxMFA(ctx context.Context, tx *sql.Tx, owner, code string, entry mailboxMFA, now time.Time) error {
	code = strings.ToLower(strings.TrimSpace(code))
	if len(code) == 6 && a.dkimCipher != nil {
		seed, err := a.dkimCipher.Open(nil, nil, entry.sealedSeed, []byte(mailboxMFASeedContext))
		if err != nil {
			return errAdminMFA
		}
		defer clear(seed)
		unix, err := a.mfaUnix(ctx, tx, now)
		if err != nil {
			return err
		}
		step, matched := totpStep(seed, code, unix)
		if !matched || step <= entry.lastStep {
			return errAdminMFA
		}
		_, err = tx.ExecContext(ctx, `UPDATE mailbox_mfa SET last_step=? WHERE owner=?`, step, owner)
		return err
	}
	if len(code) != 32 {
		return errAdminMFA
	}
	var hashes []string
	if err := json.Unmarshal([]byte(entry.recoveryHashes), &hashes); err != nil {
		return err
	}
	digest := mailboxRecoveryHash(code)
	for index, hash := range hashes {
		if subtle.ConstantTimeCompare([]byte(hash), []byte(digest)) != 1 {
			continue
		}
		remaining, err := json.Marshal(slices.Delete(hashes, index, index+1))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE mailbox_mfa SET recovery_hashes=? WHERE owner=?`, string(remaining), owner)
		return err
	}
	return errAdminMFA
}

func mailboxRecoveryHash(code string) string {
	digest := adminRecoveryHash(code)
	return hex.EncodeToString(digest[:])
}

func (a *app) mailboxMFAStatus(w http.ResponseWriter, r *http.Request, owner string) {
	_, managed := a.adminMFA[owner]
	entry, err := readMailboxMFA(r.Context(), a.db, owner)
	if mailboxMFAError(w, err) {
		return
	}
	reply(w, http.StatusOK, map[string]bool{
		"enabled": managed || entry.enabled, "managed": managed, "available": a.dkimCipher != nil,
	})
}

func mailboxMFASetupContext(owner string, session []byte) []byte {
	return append([]byte("Harbor Mail mailbox TOTP setup\x00"+owner+"\x00"), session...)
}

func (a *app) prepareMailboxMFA(w http.ResponseWriter, r *http.Request, owner string) {
	if _, managed := a.adminMFA[owner]; managed {
		fail(w, http.StatusConflict, "此账号的两步验证由服务器管理")
		return
	}
	if a.dkimCipher == nil {
		fail(w, http.StatusServiceUnavailable, "服务器尚未配置密钥保护，暂时无法开启两步验证")
		return
	}
	source := loginSource(r, a.webLogin.proxies)
	if retry := a.webLogin.reserve(r.Context(), source, owner, true); retry > 0 {
		a.rejectWebLoginRate(w, source, owner, retry)
		return
	}
	entry, err := readMailboxMFA(r.Context(), a.db, owner)
	if mailboxMFAError(w, err) {
		return
	}
	if entry.enabled {
		fail(w, http.StatusConflict, "两步验证已开启")
		return
	}
	payload := make([]byte, 8+mailboxMFASeedSize)
	binary.BigEndian.PutUint64(payload, uint64(time.Now().Add(10*time.Minute).Unix()))
	if _, err := rand.Read(payload[8:]); mailboxMFAError(w, err) {
		return
	}
	defer clear(payload)
	secret := totpBase32.EncodeToString(payload[8:])
	params := url.Values{"secret": {secret}, "issuer": {"Harbor Mail"}, "algorithm": {"SHA1"}, "digits": {"6"}, "period": {"30"}}
	uri := "otpauth://totp/" + url.PathEscape("Harbor Mail:"+owner) + "?" + params.Encode()
	qr, err := qrcode.New(uri, qrcode.Medium)
	if mailboxMFAError(w, err) {
		return
	}
	setup := a.dkimCipher.Seal(nil, nil, payload, mailboxMFASetupContext(owner, browserSessionDigest(r)))
	reply(w, http.StatusOK, map[string]any{"secret": secret, "qr": qr.Bitmap(), "setup": base64.RawURLEncoding.EncodeToString(setup)})
}

func (a *app) enableMailboxMFA(w http.ResponseWriter, r *http.Request, owner string) {
	a.changeMailboxMFA(w, r, owner, true)
}

func (a *app) disableMailboxMFA(w http.ResponseWriter, r *http.Request, owner string) {
	a.changeMailboxMFA(w, r, owner, false)
}

func (a *app) changeMailboxMFA(w http.ResponseWriter, r *http.Request, owner string, enable bool) {
	var input struct{ Password, Code, Setup string }
	if !decode(w, r, &input) {
		return
	}
	if _, managed := a.adminMFA[owner]; managed {
		fail(w, http.StatusConflict, "此账号的两步验证由服务器管理")
		return
	}
	ctx := r.Context()
	source := loginSource(r, a.webLogin.proxies)
	if retry := a.webLogin.reserve(ctx, source, owner, true); retry > 0 {
		a.rejectWebLoginRate(w, source, owner, retry)
		return
	}
	_, verified, err := a.authenticatePrimary(ctx, owner, input.Password)
	if errors.Is(err, errLoginInvalid) {
		fail(w, http.StatusForbidden, "主邮箱密码不正确")
		return
	}
	if errors.Is(err, errLoginBusy) {
		fail(w, http.StatusTooManyRequests, "验证请求过多，请稍后重试")
		return
	}
	if mailboxMFAError(w, err) {
		return
	}
	session := browserSessionDigest(r)
	tx, err := a.db.BeginTx(ctx, nil)
	if mailboxMFAError(w, err) {
		return
	}
	defer tx.Rollback()
	if mailboxMFAError(w, a.requireActiveAccount(ctx, tx, owner)) ||
		mailboxMFAError(w, a.lockBrowserSession(ctx, tx, owner, session)) ||
		mailboxMFAError(w, a.checkPrimaryPassword(ctx, tx, owner, verified)) {
		return
	}
	entry, err := readMailboxMFA(ctx, tx, owner)
	if mailboxMFAError(w, err) {
		return
	}
	if enable == entry.enabled {
		fail(w, http.StatusConflict, "两步验证状态已变更，请重新打开设置")
		return
	}
	var recovery []string
	if enable {
		recovery, err = a.saveMailboxMFA(ctx, tx, owner, session, input.Setup, input.Code)
	} else {
		err = a.consumeMailboxMFA(ctx, tx, owner, input.Code, entry, time.Now())
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM mailbox_mfa WHERE owner=?`, owner)
		}
	}
	if mailboxMFAError(w, err) {
		return
	}
	for _, table := range []string{"sessions", "web_session_details"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE owner=? AND digest<>?", owner, session); mailboxMFAError(w, err) {
			return
		}
	}
	if !mailboxMFAError(w, a.commitMailbox(tx, owner)) {
		reply(w, http.StatusOK, map[string]any{"enabled": enable, "recoveryCodes": recovery})
	}
}

var errMailboxMFASetup = errors.New("invalid or expired two-factor setup")

func (a *app) saveMailboxMFA(ctx context.Context, tx *sql.Tx, owner string, session []byte, setup, code string) ([]string, error) {
	if a.dkimCipher == nil || len(setup) > 100 {
		return nil, errMailboxMFASetup
	}
	sealed, err := base64.RawURLEncoding.DecodeString(setup)
	if err != nil {
		return nil, errMailboxMFASetup
	}
	payload, err := a.dkimCipher.Open(nil, nil, sealed, mailboxMFASetupContext(owner, session))
	if err != nil || len(payload) != 8+mailboxMFASeedSize {
		return nil, errMailboxMFASetup
	}
	defer clear(payload)
	now := time.Now()
	if int64(binary.BigEndian.Uint64(payload)) <= now.Unix() {
		return nil, errMailboxMFASetup
	}
	unix, err := a.mfaUnix(ctx, tx, now)
	if err != nil {
		return nil, err
	}
	seed := payload[8:]
	step, valid := totpStep(seed, strings.TrimSpace(code), unix)
	if !valid {
		return nil, errAdminMFA
	}
	codes, hashes := make([]string, 8), make([]string, 8)
	for index := range codes {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		codes[index] = hex.EncodeToString(random[:])
		hashes[index] = mailboxRecoveryHash(codes[index])
	}
	encoded, err := json.Marshal(hashes)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_mfa(owner,sealed_seed,recovery_hashes,last_step) VALUES(?,?,?,?)`,
		owner, a.dkimCipher.Seal(nil, nil, seed, []byte(mailboxMFASeedContext)), string(encoded), step)
	return codes, err
}

func mailboxMFAError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errAdminMFA):
		fail(w, http.StatusPreconditionRequired, "请输入有效且未使用的验证码或恢复码")
	case errors.Is(err, errMailboxMFASetup):
		fail(w, http.StatusConflict, "设置已失效，请重新扫描二维码")
	case errors.Is(err, errAccountSuspended), errors.Is(err, errBrowserSessionEnded), errors.Is(err, errPrimaryPasswordChanged):
		fail(w, http.StatusUnauthorized, "请使用当前密码重新登录")
	default:
		fail(w, http.StatusInternalServerError, "无法管理两步验证")
	}
	return true
}
