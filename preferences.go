package main

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
)

type mailboxPreferences struct {
	Revision    int64  `json:"revision"`
	Theme       string `json:"theme"`
	Density     string `json:"density"`
	ReadMode    string `json:"readMode"`
	UndoSeconds int    `json:"undoSeconds"`
}

func (p mailboxPreferences) valid() bool {
	return p.Revision >= 0 &&
		(p.Theme == "light" || p.Theme == "dark" || p.Theme == "system") &&
		(p.Density == "comfortable" || p.Density == "compact") &&
		(p.ReadMode == "auto" || p.ReadMode == "manual") &&
		(p.UndoSeconds == 0 || validUndoDelay(p.UndoSeconds))
}

func (a *app) getMailboxPreferences(w http.ResponseWriter, r *http.Request, owner string) {
	p := mailboxPreferences{Theme: "light", Density: "comfortable", ReadMode: "auto"}
	err := a.db.QueryRowContext(r.Context(), `SELECT p.revision,p.theme,p.density,p.read_mode,coalesce(s.delay_seconds,0)
		FROM mailbox_preferences p LEFT JOIN mailbox_send_delay s ON p.owner=s.owner WHERE p.owner=?`, owner).
		Scan(&p.Revision, &p.Theme, &p.Density, &p.ReadMode, &p.UndoSeconds)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Print("read mailbox preferences: ", err)
		fail(w, http.StatusInternalServerError, "无法读取邮箱偏好")
		return
	}
	reply(w, http.StatusOK, p)
}

func (a *app) putMailboxPreferences(w http.ResponseWriter, r *http.Request, owner string) {
	var input struct {
		mailboxPreferences
		UndoSeconds *int `json:"undoSeconds"`
	}
	if !decode(w, r, &input) {
		return
	}
	p := input.mailboxPreferences
	if input.UndoSeconds != nil {
		p.UndoSeconds = *input.UndoSeconds
	}
	if !p.valid() {
		fail(w, http.StatusBadRequest, "请选择有效的邮箱偏好")
		return
	}
	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if mailboxPreferenceError(w, err) {
		return
	}
	defer tx.Rollback()
	if p.Revision == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_preferences(owner,revision,theme,density,read_mode) VALUES(?,1,?,?,?)`,
			owner, p.Theme, p.Density, p.ReadMode)
	} else {
		result, updateErr := tx.ExecContext(ctx, `UPDATE mailbox_preferences SET revision=revision+1,theme=?,density=?,read_mode=?
			WHERE owner=? AND revision=?`, p.Theme, p.Density, p.ReadMode, owner, p.Revision)
		err = requireAffectedRow(result, updateErr)
	}
	if mailboxPreferenceError(w, err) {
		return
	}
	if input.UndoSeconds == nil {
		err = tx.QueryRowContext(ctx, `SELECT delay_seconds FROM mailbox_send_delay WHERE owner=?`, owner).Scan(&p.UndoSeconds)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			mailboxPreferenceError(w, err)
			return
		}
	} else {
		_, err = tx.ExecContext(ctx, `DELETE FROM mailbox_send_delay WHERE owner=?`, owner)
		if mailboxPreferenceError(w, err) {
			return
		}
		if p.UndoSeconds != 0 {
			_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_send_delay(owner,delay_seconds) VALUES(?,?)`, owner, p.UndoSeconds)
			if mailboxPreferenceError(w, err) {
				return
			}
		}
	}
	if !mailboxPreferenceError(w, a.commitMailbox(tx, owner)) {
		p.Revision++
		reply(w, http.StatusOK, p)
	}
}

func mailboxPreferenceError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, sql.ErrNoRows) || uniqueConflict(err):
		fail(w, http.StatusConflict, "邮箱偏好已在其他窗口修改，请关闭并重新打开后再编辑")
	case err != nil:
		log.Print("save mailbox preferences: ", err)
		fail(w, http.StatusInternalServerError, "无法保存邮箱偏好")
	}
	return true
}
