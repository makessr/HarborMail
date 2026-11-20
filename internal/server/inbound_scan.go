package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/emersion/go-smtp"
)

type inboundScanPolicy struct {
	rejectSpam, deferUnavailable bool
	virus                        string
}

func loadInboundScanPolicy(configured bool) (inboundScanPolicy, error) {
	var policy inboundScanPolicy
	switch env("HARBOR_SPAM_ACTION", "junk") {
	case "junk":
	case "reject":
		policy.rejectSpam = true
	default:
		return policy, errors.New("HARBOR_SPAM_ACTION must be junk or reject")
	}
	switch env("HARBOR_SCAN_FAILURE", "deliver") {
	case "deliver":
	case "defer":
		policy.deferUnavailable = true
	default:
		return policy, errors.New("HARBOR_SCAN_FAILURE must be deliver or defer")
	}
	policy.virus = os.Getenv("HARBOR_VIRUS_ACTION")
	switch policy.virus {
	case "", "junk", "reject", "quarantine":
	default:
		return policy, errors.New("HARBOR_VIRUS_ACTION must be junk, reject or quarantine")
	}
	if !configured && (policy.rejectSpam || policy.deferUnavailable || policy.virus == "reject" || policy.virus == "quarantine") {
		return policy, errors.New("strict inbound scan policies require HARBOR_RSPAMD_URL")
	}
	return policy, nil
}

func (s *delivery) scanIncoming(raw []byte) (string, error) {
	if s.app.spamScanner == nil {
		return "inbox", nil
	}
	policy := s.app.scanPolicy
	v, err := s.app.spamScanner.check(s, raw)
	if err != nil || v.unavailable {
		log.Print("Inbound scanner unavailable or incomplete")
	}
	if v.virus {
		switch policy.virus {
		case "reject":
			return "", &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "Message rejected by antivirus policy"}
		case "quarantine":
			if err := s.quarantineIncoming(raw); err != nil {
				log.Print("Inbound quarantine: ", err)
				return "", &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Quarantine temporarily unavailable"}
			}
			return "", nil
		}
	}
	if (err != nil || v.unavailable) && policy.deferUnavailable {
		return "", &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 7, 1}, Message: "Message scan temporarily unavailable"}
	}
	if policy.rejectSpam && v.reject && !v.virus {
		return "", &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "Message rejected by spam policy"}
	}
	if v.spam || v.virus {
		return "junk", nil
	}
	return "inbox", nil
}

type quarantinedRecipient struct {
	Address string
	Owner   string
}

func (s *delivery) quarantineIncoming(raw []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := s.app.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return err
	}
	var count, size int64
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(mail_size),0) FROM inbound_quarantine").Scan(&count, &size); err != nil {
		return err
	}
	if count >= 1000 || size+int64(len(raw)) > 256<<20 {
		return errors.New("quarantine capacity reached")
	}
	recipients := make([]quarantinedRecipient, 0, len(s.recipients))
	for _, r := range s.recipients {
		if err := s.app.checkDestination(ctx, tx, r.address, r.owner); err != nil {
			return err
		}
		recipients = append(recipients, quarantinedRecipient{r.address, r.owner})
	}
	encoded, err := json.Marshal(recipients)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO inbound_quarantine(sender,recipients,stamp,mail_size,raw) VALUES(?,?,?,?,?)",
		s.sender, string(encoded), time.Now().Unix(), len(raw), raw); err != nil {
		return err
	}
	return tx.Commit()
}

type quarantineItem struct {
	ID         int64                  `json:"id"`
	Sender     string                 `json:"sender"`
	Recipients []quarantinedRecipient `json:"recipients"`
	Stamp      int64                  `json:"stamp"`
	Size       int64                  `json:"size"`
}

func (a *app) adminQuarantine(w http.ResponseWriter, r *http.Request, actor string) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	rows, err := a.db.QueryContext(r.Context(), `SELECT id,sender,recipients,stamp,mail_size
		FROM inbound_quarantine WHERE (?=0 OR id<?) ORDER BY id DESC LIMIT 100`, before, before)
	if letterError(w, err) {
		return
	}
	defer rows.Close()
	items := make([]quarantineItem, 0)
	for rows.Next() {
		var item quarantineItem
		var recipients string
		if err := rows.Scan(&item.ID, &item.Sender, &recipients, &item.Stamp, &item.Size); letterError(w, err) {
			return
		}
		if letterError(w, json.Unmarshal([]byte(recipients), &item.Recipients)) {
			return
		}
		items = append(items, item)
	}
	if letterError(w, rows.Err()) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	reply(w, http.StatusOK, map[string]any{"items": items, "email": actor})
}

func (a *app) adminChangeQuarantine(w http.ResponseWriter, r *http.Request, actor string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		fail(w, 400, "无效的隔离邮件编号")
		return
	}
	var input struct{ Action, Password string }
	if !decode(w, r, &input) {
		return
	}
	if input.Action != "discard" && input.Action != "release" {
		fail(w, 400, "请选择处置方式")
		return
	}
	if _, err := a.authenticate(actor, input.Password); err != nil {
		if errors.Is(err, errLoginBusy) {
			fail(w, 429, "验证请求过多，请稍后重试")
			return
		}
		fail(w, 403, "管理员密码验证失败")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
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
	var owners []string
	if input.Action == "release" {
		var sender, encoded string
		var raw []byte
		err = tx.QueryRowContext(ctx, "SELECT sender,recipients,raw FROM inbound_quarantine WHERE id=?", id).Scan(&sender, &encoded, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			fail(w, 404, "隔离邮件已处置")
			return
		}
		if letterError(w, err) {
			return
		}
		var recipients []quarantinedRecipient
		if letterError(w, json.Unmarshal([]byte(encoded), &recipients)) {
			return
		}
		m := interpret(raw, sender, "")
		for _, recipient := range recipients {
			if err := a.checkDestination(ctx, tx, recipient.Address, recipient.Owner); err != nil {
				if errors.Is(err, errDestinationChanged) {
					fail(w, 409, "原收件地址已变化，无法释放")
				} else {
					letterError(w, err)
				}
				return
			}
			m.To = recipient.Address
			if err := insert(tx, recipient.Owner, m, "junk", raw); err != nil {
				if isQuotaExceeded(err) {
					fail(w, 507, "原收件邮箱配额不足")
				} else {
					letterError(w, err)
				}
				return
			}
			owners = append(owners, recipient.Owner)
		}
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM inbound_quarantine WHERE id=?", id)
	if letterError(w, err) {
		return
	}
	n, err := result.RowsAffected()
	if letterError(w, err) {
		return
	}
	if n == 0 {
		fail(w, 404, "隔离邮件已处置")
		return
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_account_actions(actor,target,action,affected,stamp) VALUES(?,?,?,?,?)`,
		actor, "quarantine/"+strconv.FormatInt(id, 10), "quarantine_"+input.Action, len(owners), time.Now().Unix()); letterError(w, err) {
		return
	}
	if letterError(w, a.commitMailbox(tx, owners...)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
