package server

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"time"
)

type domainDKIMStatus struct {
	Available bool           `json:"available"`
	Active    *dkimPublicKey `json:"active,omitempty"`
	Pending   *dkimPublicKey `json:"pending,omitempty"`
}

func (a *app) knownDKIMDomain(ctx context.Context, db folderQueryer, domain string) (bool, error) {
	if !validDNSName(domain) || len("harbor-0000000000000000._domainkey."+domain) > 253 {
		return false, nil
	}
	var registered bool
	err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM managed_domains WHERE name=?)`, domain).Scan(&registered)
	if err != nil || registered {
		return registered, err
	}
	return a.domainHasAddresses(ctx, db, domain)
}

func (a *app) adminDKIMStatus(w http.ResponseWriter, r *http.Request, _ string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	domain := r.PathValue("domain")
	known, err := a.knownDKIMDomain(ctx, a.db, domain)
	if letterError(w, err) {
		return
	}
	if !known {
		fail(w, http.StatusNotFound, "未配置这个邮件域名")
		return
	}
	status, err := a.readDKIMStatus(ctx, domain)
	if !letterError(w, err) {
		w.Header().Set("Cache-Control", "no-store")
		reply(w, http.StatusOK, status)
	}
}

func (a *app) readDKIMStatus(ctx context.Context, domain string) (domainDKIMStatus, error) {
	status := domainDKIMStatus{Available: a.dkimCipher != nil}
	rows, err := a.db.QueryContext(ctx, `SELECT slot,selector,public_key FROM domain_dkim_keys WHERE domain=?`, domain)
	if err != nil {
		return status, err
	}
	defer rows.Close()
	for rows.Next() {
		var slot, selector, public string
		if err := rows.Scan(&slot, &selector, &public); err != nil {
			return status, err
		}
		info := dkimDNSKey(domain, selector, public)
		if slot == "active" {
			status.Active = &info
		} else {
			status.Pending = &info
		}
	}
	return status, rows.Err()
}

func (a *app) adminChangeDKIM(w http.ResponseWriter, r *http.Request, actor string) {
	var input struct{ Action, Selector, Password string }
	if !decode(w, r, &input) {
		return
	}
	if input.Action != "generate" && input.Action != "enable" && input.Action != "disable" {
		fail(w, http.StatusBadRequest, "无效的 DKIM 操作")
		return
	}
	if a.dkimCipher == nil && input.Action != "disable" {
		fail(w, http.StatusServiceUnavailable, "请先在服务器配置 DKIM 加密密钥")
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
	domain := r.PathValue("domain")
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	known, err := a.knownDKIMDomain(ctx, a.db, domain)
	if letterError(w, err) {
		return
	}
	if !known {
		fail(w, http.StatusNotFound, "未配置这个邮件域名")
		return
	}
	var selector, public string
	var sealed []byte
	switch input.Action {
	case "generate":
		// Bound concurrent RSA key generation by the same capacity as web logins.
		select {
		case loginSlots <- struct{}{}:
			selector, public, sealed, err = generateDKIMKey(a.dkimCipher, domain)
			<-loginSlots
		default:
			fail(w, http.StatusTooManyRequests, "密钥生成繁忙，请稍后重试")
			return
		}
	case "enable":
		err = a.db.QueryRowContext(ctx, `SELECT selector,public_key,sealed_key FROM domain_dkim_keys
			WHERE domain=? AND slot='pending' AND selector=?`, domain, input.Selector).Scan(&selector, &public, &sealed)
		if errors.Is(err, sql.ErrNoRows) {
			fail(w, http.StatusConflict, "待启用的选择器已变更，请刷新后重试")
			return
		}
		if err == nil {
			_, err = openDKIMKey(a.dkimCipher, domain, selector, sealed)
		}
		if err == nil {
			dnsCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			state := checkDKIMPublicKey(dnsCtx, net.DefaultResolver, dkimDNSKey(domain, selector, public).Name, public)
			stop()
			if state != "match" {
				fail(w, http.StatusConflict, "DNS 尚未返回匹配的 DKIM TXT 记录")
				return
			}
		}
	}
	if errors.Is(err, errDKIMSigning) {
		fail(w, http.StatusServiceUnavailable, "无法解密 DKIM 私钥，请核对服务器的加密密钥")
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
	if letterError(w, lockIMAPUIDs(ctx, tx)) || browserSessionError(w, a.lockBrowserSession(ctx, tx, actor, browserSessionDigest(r))) {
		return
	}
	known, err = a.knownDKIMDomain(ctx, tx, domain)
	if letterError(w, err) {
		return
	}
	if !known {
		fail(w, http.StatusNotFound, "未配置这个邮件域名")
		return
	}
	if err := changeDomainDKIM(ctx, tx, domain, input.Action, input.Selector, selector, public, sealed); errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusConflict, "DKIM 配置已变更，请刷新后重试")
		return
	} else if letterError(w, err) {
		return
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_account_actions(actor,target,action,affected,stamp)
		VALUES(?,?,?,?,?)`, actor, domain, "dkim_"+input.Action, 0, time.Now().Unix())
	if letterError(w, err) || letterError(w, tx.Commit()) {
		return
	}
	a.adminDKIMStatus(w, r, actor)
}

func changeDomainDKIM(ctx context.Context, tx *sql.Tx, domain, action, expected, selector, public string, sealed []byte) error {
	switch action {
	case "generate":
		if _, err := tx.ExecContext(ctx, `DELETE FROM domain_dkim_keys WHERE domain=? AND slot='pending'`, domain); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO domain_dkim_keys(domain,slot,selector,public_key,sealed_key) VALUES(?,'pending',?,?,?)`,
			domain, selector, public, sealed)
		return err
	case "enable":
		if _, err := tx.ExecContext(ctx, `DELETE FROM domain_dkim_keys WHERE domain=? AND slot='active'`, domain); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE domain_dkim_keys SET slot='active' WHERE domain=? AND slot='pending' AND selector=?`, domain, expected)
		return requireAffectedRow(result, err)
	default: // disable
		result, err := tx.ExecContext(ctx, `DELETE FROM domain_dkim_keys WHERE domain=? AND slot='active' AND selector=?`, domain, expected)
		return requireAffectedRow(result, err)
	}
}
