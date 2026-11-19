package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var errDraftConflict = errors.New("draft revision changed")

func (a *app) createDraft(w http.ResponseWriter, r *http.Request, owner string) {
	a.saveWebDraft(w, r, owner, false)
}

func (a *app) editDraft(w http.ResponseWriter, r *http.Request, owner string) {
	a.saveWebDraft(w, r, owner, true)
}

func (a *app) saveWebDraft(w http.ResponseWriter, r *http.Request, owner string, existing bool) {
	key := ""
	if !existing {
		key = r.Header.Get("Idempotency-Key")
		if key != "" {
			if _, err := hex.DecodeString(key); err != nil || len(key) != 32 || key != strings.ToLower(key) {
				fail(w, 400, "草稿保存请求编号无效")
				return
			}
		}
	}
	var input outgoingLetter
	if !decodeOutgoing(w, r, &input) {
		return
	}
	fields := input.recipientFields()
	if !fields.validFields() || strings.ContainsAny(input.Subject, "\r\n") ||
		len(input.Subject) > 300 || len(input.Body) > 256<<10 {
		fail(w, 400, "收件人、主题或正文超出草稿限制")
		return
	}
	if envelope, err := a.resolveWebEnvelope(r.Context(), a.db, fields); err == nil {
		fields = envelope.Recipients
	}
	m := letter{From: owner, To: fields.indexedAddress(), Subject: input.Subject,
		Body: input.Body, Date: time.Now().Unix(), Read: true, recipients: &fields}
	wire := m
	ctx := r.Context()
	id, expected := letterID(r), draftRevision(r)
	if existing && expected < 1 {
		fail(w, 428, "请重新打开草稿后再保存")
		return
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if letterError(w, err) {
		return
	}
	defer tx.Rollback()
	if letterError(w, lockIMAPUIDs(ctx, tx)) || letterError(w, a.requireActiveAccount(ctx, tx, owner)) {
		return
	}
	if key != "" {
		var folder string
		err := tx.QueryRowContext(ctx, `SELECT k.letter_id,coalesce(l.folder,'') FROM draft_creation_keys k
			LEFT JOIN letters l ON l.id=k.letter_id AND l.owner=k.owner WHERE k.owner=? AND k.request_key=?`,
			owner, key).Scan(&m.ID, &folder)
		switch {
		case err == nil && folder != "drafts":
			fail(w, 409, "这份草稿已移出草稿箱，请检查邮箱后再编辑")
			return
		case err == nil:
			reply(w, 200, map[string]int64{"id": m.ID, "draftVersion": 1})
			return
		case !errors.Is(err, sql.ErrNoRows):
			letterError(w, err)
			return
		}
	}
	if existing {
		if draftError(w, advanceDraftRevision(ctx, tx, owner, id, expected)) {
			return
		}
		var previous []byte
		if err := tx.QueryRowContext(ctx, `SELECT substr(raw,1,?) FROM letters WHERE id=? AND owner=? AND folder='drafts'`,
			threadHeaderLimit, id, owner).Scan(&previous); letterError(w, err) {
			return
		}
		wire.reply = identifyMessage(previous).replyHeaders
	} else {
		wire.reply, err = prepareReply(ctx, tx, owner, input.ReplyToID)
		if letterError(w, err) {
			return
		}
	}
	raw, err := encodeLetter(wire, a.domain, input.files)
	if letterError(w, err) {
		return
	}
	if len(raw) > mailLimit {
		fail(w, 413, "编码后的草稿超出 10 MiB")
		return
	}
	if existing {
		if _, err := tx.ExecContext(ctx, `UPDATE letters SET recipient=?,subject=?,body=?,stamp=?,raw=?
			WHERE id=? AND owner=? AND folder='drafts'`, m.To, m.Subject, m.Body, m.Date, raw, id, owner); letterError(w, err) {
			return
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM web_letter_recipients WHERE letter_id=?`, id); letterError(w, err) {
			return
		}
		if letterError(w, saveWebRecipients(tx, id, fields)) {
			return
		}
		if letterError(w, clearIMAPMembership(ctx, tx, id)) {
			return
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM mail_threads WHERE letter_id=? AND owner=?`, id, owner); letterError(w, err) {
			return
		}
		if letterError(w, indexThread(tx, owner, id, raw)) {
			return
		}
		m.ID, m.DraftVersion = id, expected+1
	} else {
		draftID, err := insertWebDraft(ctx, tx, owner, m, raw)
		if letterError(w, err) {
			return
		}
		m.ID = draftID
		m.DraftVersion = 1
		if key != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO draft_creation_keys(owner,request_key,letter_id) VALUES(?,?,?)`,
				owner, key, draftID); letterError(w, err) {
				return
			}
		}
	}
	if letterError(w, a.commitMailbox(tx, owner)) {
		return
	}
	status := 200
	if !existing {
		status = 201
	}
	reply(w, status, map[string]int64{"id": m.ID, "draftVersion": m.DraftVersion})
}

func insertWebDraft(ctx context.Context, tx *sql.Tx, owner string, m letter, raw []byte) (int64, error) {
	id, err := insertLetter(tx, owner, m, "drafts", "drafts", raw)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO web_drafts (letter_id,revision) VALUES(?,1)`, id)
	return id, err
}

func draftRevision(r *http.Request) int64 {
	revision, _ := strconv.ParseInt(strings.Trim(r.Header.Get("If-Match"), `"`), 10, 64)
	return revision
}

func advanceDraftRevision(ctx context.Context, tx *sql.Tx, owner string, id, expected int64) error {
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT d.revision FROM web_drafts d JOIN letters l ON l.id=d.letter_id
		WHERE l.id=? AND l.owner=? AND l.folder='drafts'`, id, owner).Scan(&revision)
	if err != nil {
		return err
	}
	if revision != expected {
		return errDraftConflict
	}
	_, err = tx.ExecContext(ctx, `UPDATE web_drafts SET revision=revision+1 WHERE letter_id=?`, id)
	return err
}

func draftError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, errDraftConflict) {
		fail(w, 409, "草稿已在另一处修改，请重新打开后合并更改")
		return true
	}
	return letterError(w, err)
}

func (a *app) sendDraft(w http.ResponseWriter, r *http.Request, owner string) {
	plan, valid := parseSendPlan(w, r)
	if !valid {
		return
	}
	id, expected := letterID(r), draftRevision(r)
	if expected < 1 {
		fail(w, 428, "请重新打开草稿后再发送")
		return
	}
	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if letterError(w, err) {
		return
	}
	defer tx.Rollback()
	if letterError(w, lockIMAPUIDs(ctx, tx)) || draftError(w, advanceDraftRevision(ctx, tx, owner, id, expected)) {
		return
	}
	m := letter{From: owner, Date: time.Now().Unix()}
	var raw []byte
	var fields webRecipients
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(e.to_list,l.recipient),coalesce(e.cc_list,''),coalesce(e.bcc_list,''),
		l.subject,l.body,l.raw FROM letters l LEFT JOIN web_letter_recipients e ON e.letter_id=l.id
		WHERE l.owner=? AND l.id=? AND l.folder='drafts'`, owner, id).
		Scan(&fields.To, &fields.Cc, &fields.Bcc, &m.Subject, &m.Body, &raw); letterError(w, err) {
		return
	}
	if strings.TrimSpace(m.Body) == "" {
		fail(w, 400, "请填写邮件正文")
		return
	}
	envelope, valid := a.webDeliveryEnvelope(w, ctx, tx, owner, fields)
	if !valid {
		return
	}
	m.recipients = &envelope.Recipients
	m.To = envelope.Recipients.indexedAddress()
	if plan.at != 0 {
		m.Date = plan.at
	}
	raw, err = rewriteOutgoingHeaders(raw, m.Date, m.recipients)
	if letterError(w, err) {
		return
	}
	if len(raw) > mailLimit {
		fail(w, 413, "编码后的邮件超出 10 MiB，请减少附件或正文")
		return
	}
	if _, err := tx.ExecContext(ctx, `UPDATE letters SET folder='trash',previous='drafts'
		WHERE owner=? AND id=? AND folder='drafts'`, owner, id); letterError(w, err) {
		return
	}
	if letterError(w, clearIMAPMembership(ctx, tx, id)) {
		return
	}
	a.submitWebMail(w, ctx, tx, m, raw, envelope, plan)
}
