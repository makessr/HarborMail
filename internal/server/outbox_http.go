package server

import (
	"net/http"
	"strconv"
	"strings"
)

type outboxScanner interface{ Scan(...any) error }

func scanOutbox(row outboxScanner) (letter, error) {
	m := letter{Folder: "outbox", Read: true, Delivery: new(deliveryStatus)}
	var envelopeData string
	err := row.Scan(&m.ID, &m.From, &m.To, &m.Subject, &m.Body, &m.Date,
		&m.Delivery.State, &m.Delivery.Attempts, &m.Delivery.Due, &m.Delivery.Reason, &m.Delivery.SendAt, &envelopeData)
	if err != nil {
		return m, err
	}
	envelope, err := parseWebEnvelope(envelopeData)
	if err != nil {
		return m, err
	}
	if envelope != nil {
		m.showWebRecipients(envelope.Recipients)
		if envelope.Author != "" {
			m.From = envelope.Author
		}
		if envelope.Formatted {
			m.Body, _ = renderRichCompose(m.Body)
		}
	}
	return m, nil
}

func updateOutboxUndoStatus(status *deliveryStatus) {
	if status.State != "undo" {
		return
	}
	remaining := undoRemainingMs(status.Due)
	if remaining == 0 {
		// The worker still uses the stored undo row to finish delivery.
		status.State = "queued"
		status.SendAt = 0
		return
	}
	status.UndoRemainingMs = &remaining
}

func (a *app) listOutbox(w http.ResponseWriter, r *http.Request, owner string) {
	query := `SELECT id,sender,recipient,subject,substr(body,1,180),stamp,state,attempts,due,reason,
		coalesce(s.send_at,0),coalesce(e.envelope,'') FROM outbox LEFT JOIN outbox_schedule s ON s.outbox_id=outbox.id
		LEFT JOIN web_outbox_envelopes e ON e.outbox_id=outbox.id WHERE sender=?`
	args := []any{owner}
	if search := strings.TrimSpace(r.URL.Query().Get("q")); search != "" {
		query += ` AND instr(lower(concat(recipient, ' ', subject, ' ', body)), lower(?))>0`
		args = append(args, search)
	}
	if before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64); before > 0 {
		query += ` AND id<?`
		args = append(args, before)
	}
	rows, err := a.db.QueryContext(r.Context(), query+` ORDER BY id DESC LIMIT 51`, args...)
	if letterError(w, err) {
		return
	}
	defer rows.Close()
	list := make([]letter, 0, 51)
	for rows.Next() {
		m, err := scanOutbox(rows)
		if letterError(w, err) {
			return
		}
		list = append(list, m)
	}
	if letterError(w, rows.Err()) {
		return
	}
	var next int64
	if len(list) == 51 {
		list = list[:50]
		next = list[49].ID
	}
	for _, item := range list {
		updateOutboxUndoStatus(item.Delivery)
	}
	reply(w, 200, map[string]any{"letters": list, "next": next})
}

func (a *app) readOutbox(w http.ResponseWriter, r *http.Request, owner string) {
	m, err := scanOutbox(a.db.QueryRowContext(r.Context(), `SELECT id,sender,recipient,subject,body,stamp,state,attempts,due,reason,
		coalesce(s.send_at,0),coalesce(e.envelope,'') FROM outbox LEFT JOIN outbox_schedule s ON s.outbox_id=outbox.id
		LEFT JOIN web_outbox_envelopes e ON e.outbox_id=outbox.id WHERE sender=? AND id=?`, owner, letterID(r)))
	if letterError(w, err) {
		return
	}
	m.Delivery.Recipients, err = readRecipientProgress(r.Context(), a.db, owner, m.ID)
	if letterError(w, err) {
		return
	}
	m.Timeline, err = readDeliveryTimeline(r.Context(), a.db, owner, m.ID)
	if !letterError(w, err) {
		updateOutboxUndoStatus(m.Delivery)
		reply(w, http.StatusOK, m)
	}
}

func (a *app) downloadOutbox(w http.ResponseWriter, r *http.Request, owner string) {
	id := letterID(r)
	var raw []byte
	err := a.db.QueryRowContext(r.Context(), `SELECT raw FROM outbox WHERE sender=? AND id=?`, owner, id).Scan(&raw)
	if !letterError(w, err) {
		serveRawMail(w, r, id, raw)
	}
}
