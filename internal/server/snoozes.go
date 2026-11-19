package server

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"time"
)

type snoozeChange struct {
	ResumeAt   int64 `json:"resumeAt"`
	PreviousAt int64 `json:"previousAt"`
}

var errSnoozeChanged = errors.New("mail snooze state changed")

func (a *app) setSnooze(w http.ResponseWriter, r *http.Request, owner string) {
	var change snoozeChange
	if !decode(w, r, &change) {
		return
	}
	now := time.Now()
	if change.PreviousAt < 0 || (change.ResumeAt == 0 && change.PreviousAt == 0) ||
		(change.ResumeAt != 0 && (change.ResumeAt <= now.Unix() || change.ResumeAt > now.Add(365*24*time.Hour).Unix())) {
		fail(w, http.StatusBadRequest, "请选择未来一年内的移回时间")
		return
	}
	err := a.changeSnooze(r.Context(), owner, letterID(r), change)
	if errors.Is(err, errSnoozeChanged) {
		fail(w, http.StatusConflict, "邮件状态已改变，请刷新后重试")
		return
	}
	if !letterError(w, err) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *app) changeSnooze(ctx context.Context, owner string, id int64, change snoozeChange) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return err
	}
	var folder string
	var previous int64
	err = tx.QueryRowContext(ctx, `SELECT l.folder,coalesce(s.resume_at,0) FROM letters l
		LEFT JOIN mail_snoozes s ON s.letter_id=l.id WHERE l.owner=? AND l.id=?`, owner, id).Scan(&folder, &previous)
	if err != nil {
		return err
	}
	if previous != change.PreviousAt || (previous == 0 && folder != "inbox") || (previous != 0 && folder != "snoozed") {
		return errSnoozeChanged
	}
	switch {
	case change.ResumeAt == 0:
		err = restoreSnoozedLetter(ctx, tx, owner, id, previous)
	case previous != 0:
		_, err = tx.ExecContext(ctx, `UPDATE mail_snoozes SET resume_at=? WHERE letter_id=?`, change.ResumeAt, id)
	default:
		if _, err = tx.ExecContext(ctx, `INSERT INTO mail_snoozes(letter_id,resume_at) VALUES(?,?)`, id, change.ResumeAt); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE letters SET folder='snoozed',previous='' WHERE owner=? AND id=?`, owner, id); err != nil {
			return err
		}
		err = invalidateMovedUID(ctx, tx, id)
	}
	if err != nil {
		return err
	}
	return a.commitMailbox(tx, owner)
}

func restoreSnoozedLetter(ctx context.Context, tx *sql.Tx, owner string, id, resumeAt int64) error {
	result, err := tx.ExecContext(ctx, `UPDATE letters SET folder='inbox',previous='' WHERE owner=? AND id=? AND folder='snoozed'
		AND EXISTS (SELECT 1 FROM mail_snoozes WHERE letter_id=? AND resume_at=?)`, owner, id, id, resumeAt)
	if err := requireAffectedRow(result, err); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM mail_snoozes WHERE letter_id=?`, id)
	return err
}

func (a *app) restoreDueSnoozes(ctx context.Context, cutoff int64) error {
	rows, err := a.db.QueryContext(ctx, `SELECT l.id,l.owner,s.resume_at FROM mail_snoozes s JOIN letters l ON l.id=s.letter_id
		WHERE s.resume_at<=? AND l.folder='snoozed' ORDER BY s.resume_at,s.letter_id LIMIT 100`, cutoff)
	if err != nil {
		return err
	}
	type dueLetter struct {
		id, resumeAt int64
		owner        string
	}
	var due []dueLetter
	for rows.Next() {
		var item dueLetter
		if err := rows.Scan(&item.id, &item.owner, &item.resumeAt); err != nil {
			rows.Close()
			return err
		}
		due = append(due, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(due) == 0 {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return err
	}
	var owners []string
	for _, item := range due {
		err := restoreSnoozedLetter(ctx, tx, item.owner, item.id, item.resumeAt)
		if errors.Is(err, sql.ErrNoRows) {
			continue // Rescheduled or moved after the due list was read.
		}
		if err != nil {
			return err
		}
		owners = append(owners, item.owner)
	}
	return a.commitMailbox(tx, owners...)
}

func (a *app) runSnoozes(stop context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for stop.Err() == nil {
		ctx, cancel := context.WithTimeout(stop, 10*time.Second)
		err := a.restoreDueSnoozes(ctx, time.Now().Unix())
		cancel()
		if err != nil && stop.Err() == nil {
			log.Print("restore snoozed mail: ", err)
		}
		select {
		case <-stop.Done():
			return
		case <-ticker.C:
		}
	}
}
