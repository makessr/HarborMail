package server

import (
	"context"
	"net/http"
	"time"
)

func (a *app) deleteTrashedMail(w http.ResponseWriter, r *http.Request, owner string) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if letterError(w, err) {
		return
	}
	defer tx.Rollback()
	if letterError(w, lockIMAPUIDs(ctx, tx)) ||
		browserSessionError(w, a.lockBrowserSession(ctx, tx, owner, browserSessionDigest(r))) {
		return
	}
	id := letterID(r)
	result, err := tx.ExecContext(ctx, `DELETE FROM letters WHERE id=? AND owner=? AND folder='trash'`, id, owner)
	if err := requireAffectedRow(result, err); letterError(w, err) {
		return
	}
	if letterError(w, clearIMAPMembership(ctx, tx, id)) {
		return
	}
	for _, table := range []string{"draft_creation_keys", "web_drafts", "web_letter_recipients",
		"mail_threads", "mail_snoozes", "mail_label_links", "spam_feedback"} {
		_, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE letter_id=?", id)
		if letterError(w, err) {
			return
		}
	}
	if !letterError(w, a.commitMailbox(tx, owner)) {
		w.WriteHeader(http.StatusNoContent)
	}
}
