package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

func (a *app) baseAccountHash(ctx context.Context, db folderQueryer, email string) ([]byte, error) {
	if hash, exists := a.accounts[email]; exists {
		return hash, nil
	}
	var hash []byte
	err := db.QueryRowContext(ctx, `SELECT password_hash FROM managed_accounts WHERE email=?`, email).Scan(&hash)
	return hash, err
}

func (a *app) baseAccountDigest(ctx context.Context, db folderQueryer, email string) ([sha256.Size]byte, error) {
	hash, err := a.baseAccountHash(ctx, db, email)
	return sha256.Sum256(hash), err
}

func (a *app) allAccountNames(ctx context.Context) (map[string]bool, error) {
	names := make(map[string]bool, len(a.accounts))
	for email := range a.accounts {
		names[email] = true
	}
	rows, err := a.db.QueryContext(ctx, `SELECT email FROM managed_accounts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		names[email] = true
	}
	return names, rows.Err()
}

func (a *app) registerConfiguredAccounts() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT email FROM managed_accounts`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			rows.Close()
			return err
		}
		if _, exists := a.accounts[email]; exists || a.aliases[email] != "" {
			rows.Close()
			return errors.New("server accounts or aliases conflict with a database-managed primary mailbox")
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// Preserve an address reservation even if its server configuration is removed later.
	emails := make([]string, 0, len(a.accounts))
	for email := range a.accounts {
		emails = append(emails, email)
	}
	slices.Sort(emails)
	for _, email := range emails {
		if _, err := a.lockAccountAccess(ctx, tx, email); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *app) adminCreateAccount(w http.ResponseWriter, r *http.Request, actor string) {
	var input struct{ Email, InitialPassword, Password string }
	if !decode(w, r, &input) {
		return
	}
	email := strings.ToLower(strings.TrimSpace(input.Email))
	if canonical, err := address(email); err != nil || email != canonical ||
		strings.HasSuffix(email, ".") || !validDNSName(emailDomain(email)) {
		fail(w, 400, "请输入有效的主邮箱地址")
		return
	}
	if !validNewPrimaryPassword(input.InitialPassword) {
		fail(w, 400, "初始密码须为 12 至 72 字节，且不能含换行或空字符")
		return
	}
	if !a.canManageDomain(r.Context(), actor, emailDomain(email)) {
		fail(w, http.StatusForbidden, "当前账号无权在该域名创建邮箱")
		return
	}
	if _, err := a.authenticate(actor, input.Password); err != nil {
		if errors.Is(err, errLoginBusy) {
			fail(w, 429, "验证请求过多，请稍后重试")
		} else {
			fail(w, 403, "管理员主邮箱密码不正确")
		}
		return
	}
	hash, err := hashPrimaryPassword(nil, input.InitialPassword)
	if errors.Is(err, errLoginBusy) {
		fail(w, 429, "验证请求过多，请稍后重试")
		return
	}
	if letterError(w, err) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if letterError(w, err) {
		return
	}
	defer tx.Rollback()
	if letterError(w, lockIMAPUIDs(ctx, tx)) ||
		browserSessionError(w, a.lockBrowserSession(ctx, tx, actor, browserSessionDigest(r))) {
		return
	}
	target, err := a.destination(ctx, tx, email)
	if errors.Is(err, errDestinationChanged) || (err == nil && target.owner != "") {
		fail(w, 409, "此地址已用作主邮箱或收信别名")
		return
	}
	if letterError(w, err) {
		return
	}
	reserved, err := a.priorMailboxExists(ctx, tx, email)
	if letterError(w, err) {
		return
	}
	if reserved {
		fail(w, 409, "此地址有历史账号数据，请从服务器恢复原账号")
		return
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO managed_accounts(email,password_hash) VALUES(?,?)`, email, hash); letterError(w, err) {
		return
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_access(owner,suspended) VALUES(?,FALSE)`, email); letterError(w, err) {
		return
	}
	if limit := a.domainQuota[emailDomain(email)]; limit > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO mailbox_quota(owner,used_bytes,limit_mib) VALUES(?,0,?)`, email, limit); letterError(w, err) {
			return
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_account_actions(actor,target,action,affected,stamp)
		VALUES(?,?,?,?,?)`, actor, email, "account_create", 0, time.Now().Unix()); letterError(w, err) {
		return
	}
	if !letterError(w, a.commitMailbox(tx, email)) {
		reply(w, http.StatusCreated, map[string]string{"email": email})
	}
}

func (a *app) priorMailboxExists(ctx context.Context, tx *sql.Tx, email string) (bool, error) {
	tables := []string{
		"account_access", "letters", "sessions", "web_session_details", "app_passwords", "custom_folders",
		"contacts", "mail_rules", "mail_labels", "mailbox_preferences",
		"signature_settings", "mail_templates", "saved_searches", "mailbox_send_delay", "managed_aliases",
		"draft_creation_keys", "mail_threads", "account_password_overrides", "mailbox_storage_alerts", "mailbox_quota", "mailbox_retirements",
	}
	if a.sharedMailboxEvents {
		tables = append(tables, "mailbox_revisions")
	}
	for _, table := range tables {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE owner=?)`, email).Scan(&exists); err != nil || exists {
			return exists, err
		}
	}
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM outbox WHERE sender=?)`, email).Scan(&exists)
	return exists, err
}
