package server

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type mailLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

var errLabelReferenced = errors.New("mail label used by a rule")

func labelHTTPError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, sql.ErrNoRows):
		fail(w, http.StatusNotFound, "找不到标签或邮件")
	case errors.Is(err, errLabelReferenced):
		fail(w, http.StatusConflict, "此标签被邮件规则引用，请先修改或删除相关规则")
	case uniqueConflict(err):
		fail(w, http.StatusConflict, "已有同名标签")
	default:
		log.Print("mail label operation: ", err)
		fail(w, http.StatusInternalServerError, "标签操作失败")
	}
	return true
}

func (a *app) mailLabels(ctx context.Context, owner string, letter int64) ([]mailLabel, error) {
	query := `SELECT id,name FROM mail_labels WHERE owner=?`
	args := []any{owner}
	if letter > 0 {
		query += ` AND EXISTS (SELECT 1 FROM mail_label_links WHERE label_id=mail_labels.id AND letter_id=?)`
		args = append(args, letter)
	}
	rows, err := a.db.QueryContext(ctx, query+` ORDER BY name_key,id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	labels := make([]mailLabel, 0)
	for rows.Next() {
		var label mailLabel
		if err := rows.Scan(&label.ID, &label.Name); err != nil {
			return nil, err
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

func (a *app) listMailLabels(w http.ResponseWriter, r *http.Request, owner string) {
	labels, err := a.mailLabels(r.Context(), owner, 0)
	if !labelHTTPError(w, err) {
		reply(w, http.StatusOK, labels)
	}
}

func (a *app) listLetterLabels(w http.ResponseWriter, r *http.Request, owner string) {
	id := letterID(r)
	var exists int
	ctx := r.Context()
	if letterError(w, a.db.QueryRowContext(ctx, `SELECT 1 FROM letters WHERE owner=? AND id=?`, owner, id).Scan(&exists)) {
		return
	}
	labels, err := a.mailLabels(ctx, owner, id)
	if !labelHTTPError(w, err) {
		reply(w, http.StatusOK, labels)
	}
}

func (a *app) saveMailLabel(w http.ResponseWriter, r *http.Request, owner string) {
	var label mailLabel
	if !decode(w, r, &label) {
		return
	}
	label.Name = strings.TrimSpace(label.Name)
	if label.Name == "" || utf8.RuneCountInString(label.Name) > 40 || strings.ContainsFunc(label.Name, unicode.IsControl) {
		fail(w, http.StatusBadRequest, "标签名称须为 1–40 个字符，且不能包含控制字符")
		return
	}
	ctx, key := r.Context(), strings.ToLower(label.Name)
	status := http.StatusOK
	if r.Method == http.MethodPost {
		result, err := a.db.ExecContext(ctx, `INSERT INTO mail_labels(owner,name,name_key) VALUES(?,?,?)`, owner, label.Name, key)
		if labelHTTPError(w, err) {
			return
		}
		if label.ID, err = result.LastInsertId(); labelHTTPError(w, err) {
			return
		}
		status = http.StatusCreated
	} else {
		label.ID, _ = strconv.ParseInt(r.PathValue("label"), 10, 64)
		result, err := a.db.ExecContext(ctx, `UPDATE mail_labels SET name=?,name_key=? WHERE owner=? AND id=?`, label.Name, key, owner, label.ID)
		if labelHTTPError(w, requireAffectedRow(result, err)) {
			return
		}
	}
	a.events.notify(owner)
	reply(w, status, label)
}

func (a *app) deleteMailLabel(w http.ResponseWriter, r *http.Request, owner string) {
	id, _ := strconv.ParseInt(r.PathValue("label"), 10, 64)
	if !labelHTTPError(w, a.removeMailLabel(r.Context(), owner, id)) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *app) removeMailLabel(ctx context.Context, owner string, id int64) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return err
	}
	var referenced bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM mail_rules
		WHERE owner=? AND rule_action='label' AND destination=?)`, owner, strconv.FormatInt(id, 36)).Scan(&referenced); err != nil {
		return err
	}
	if referenced {
		return errLabelReferenced
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM mail_labels WHERE owner=? AND id=?`, owner, id)
	if err := requireAffectedRow(result, err); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM mail_label_links WHERE label_id=?`, id); err != nil {
		return err
	}
	return a.commitMailbox(tx, owner)
}

func (a *app) changeMailLabel(w http.ResponseWriter, r *http.Request, owner string) {
	id, _ := strconv.ParseInt(r.PathValue("label"), 10, 64)
	if !labelHTTPError(w, a.setMailLabels(r.Context(), owner, []int64{letterID(r)}, id, r.Method == http.MethodPut)) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// Callers pass letter IDs in ascending order to keep overlapping writes consistent.
func (a *app) setMailLabels(ctx context.Context, owner string, letters []int64, label int64, add bool) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Verify both resources while locking their rows so a concurrent delete or
	// retry cannot create orphaned or duplicate links on either database.
	result, err := tx.ExecContext(ctx, `UPDATE mail_labels SET id=id WHERE id=? AND owner=?`, label, owner)
	if err := requireAffectedRow(result, err); err != nil {
		return err
	}
	for _, letter := range letters {
		result, err := tx.ExecContext(ctx, `UPDATE letters SET id=id WHERE id=? AND owner=? AND folder<>'drafts'`, letter, owner)
		if err := requireAffectedRow(result, err); err != nil {
			return err
		}
		if add {
			err = attachMailLabel(ctx, tx, letter, label)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM mail_label_links WHERE letter_id=? AND label_id=?`, letter, label)
		}
		if err != nil {
			return err
		}
	}
	return a.commitMailbox(tx, owner)
}

func attachMailLabel(ctx context.Context, tx *sql.Tx, letter, label int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO mail_label_links(letter_id,label_id)
		SELECT ?,? WHERE NOT EXISTS (SELECT 1 FROM mail_label_links WHERE letter_id=? AND label_id=?)`, letter, label, letter, label)
	return err
}
