package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

func letterListFilters(params url.Values) (string, []any, bool) {
	var query strings.Builder
	var args []any
	for _, field := range []struct{ name, column string }{
		{"sender", "sender"},
		{"recipient", "concat(coalesce(e.to_list,recipient),' ',coalesce(e.cc_list,''),' ',coalesce(e.bcc_list,''))"},
	} {
		if value := strings.TrimSpace(params.Get(field.name)); value != "" {
			query.WriteString(" AND instr(lower(" + field.column + "), lower(?))>0")
			args = append(args, value)
		}
	}
	switch params.Get("seen") {
	case "read":
		query.WriteString(" AND seen=TRUE")
	case "unread":
		query.WriteString(" AND seen=FALSE")
	case "":
	default:
		return "", nil, false
	}
	since, until := params.Get("since"), params.Get("until")
	var lower, upper int64
	if since != "" {
		var err error
		if lower, err = strconv.ParseInt(since, 10, 64); err != nil {
			return "", nil, false
		}
		query.WriteString(" AND stamp>=?")
		args = append(args, lower)
	}
	if until != "" {
		var err error
		if upper, err = strconv.ParseInt(until, 10, 64); err != nil || (since != "" && lower >= upper) {
			return "", nil, false
		}
		query.WriteString(" AND stamp<?")
		args = append(args, upper)
	}
	return query.String(), args, true
}

const maxBatchLetters = 100

var (
	errInvalidMove       = errors.New("invalid mail destination")
	errMoveFolderMissing = errors.New("mail destination not found")
)

type letterChange struct {
	Folder       *string `json:"folder"`
	Read         *bool   `json:"read"`
	Star         *bool   `json:"star"`
	SpamFeedback bool    `json:"spamFeedback"`
}

func (a *app) update(w http.ResponseWriter, r *http.Request, owner string) {
	var change letterChange
	if decode(w, r, &change) {
		a.writeLetterChanges(w, r, owner, []int64{letterID(r)}, change)
	}
}

func (a *app) updateBatch(w http.ResponseWriter, r *http.Request, owner string) {
	var input struct {
		IDs []int64 `json:"ids"`
		letterChange
		Label *struct {
			ID  int64 `json:"id"`
			Add *bool `json:"add"`
		} `json:"label"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.IDs) == 0 || len(input.IDs) > maxBatchLetters {
		fail(w, 400, "请选择 1–100 封邮件")
		return
	}
	// Keep write order consistent when bulk operations overlap.
	slices.Sort(input.IDs)
	for i, id := range input.IDs {
		if id <= 0 || (i > 0 && id == input.IDs[i-1]) {
			fail(w, 400, "所选邮件包含无效或重复的编号")
			return
		}
	}
	hasOtherAction := input.Folder != nil || input.Read != nil || input.Star != nil || input.SpamFeedback
	if input.Label != nil {
		if input.Label.ID < 1 || input.Label.Add == nil || hasOtherAction {
			fail(w, 400, "请选择一个有效的标签操作")
			return
		}
		if !labelHTTPError(w, a.setMailLabels(r.Context(), owner, input.IDs, input.Label.ID, *input.Label.Add)) {
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	if !hasOtherAction {
		fail(w, 400, "请选择要执行的操作")
		return
	}
	a.writeLetterChanges(w, r, owner, input.IDs, input.letterChange)
}

func (a *app) writeLetterChanges(w http.ResponseWriter, r *http.Request, owner string, ids []int64, change letterChange) {
	err := a.changeLetters(r.Context(), owner, ids, change)
	switch {
	case errors.Is(err, errInvalidMove):
		fail(w, 400, "无效的目标文件夹")
	case errors.Is(err, errInvalidSpamFeedback):
		fail(w, 400, "请选择标记为垃圾邮件或标记为非垃圾邮件")
	case errors.Is(err, errMoveFolderMissing):
		folderHTTPError(w, sql.ErrNoRows)
	case errors.Is(err, sql.ErrNoRows) && len(ids) > 1:
		fail(w, 404, "部分所选邮件已不可用，请刷新列表后重试")
	default:
		if !letterError(w, err) {
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func (a *app) changeLetters(ctx context.Context, owner string, ids []int64, change letterChange) error {
	if change.SpamFeedback && (change.Folder == nil || (*change.Folder != "junk" && *change.Folder != "inbox")) {
		return errInvalidSpamFeedback
	}
	if change.Folder != nil {
		switch *change.Folder {
		case "restore", "archive", "trash", "inbox", "junk":
		default:
			if !customFolderID.MatchString(*change.Folder) {
				return errInvalidMove
			}
		}
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if change.Folder != nil {
		if err := lockIMAPUIDs(ctx, tx); err != nil {
			return err
		}
		if err := verifyFolder(ctx, tx, owner, *change.Folder); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errMoveFolderMissing
			}
			return err
		}
	}
	for _, id := range ids {
		if change.SpamFeedback && a.spamTrainer.allows(owner) {
			if err := enqueueSpamFeedback(ctx, tx, owner, id, *change.Folder); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE letters SET
			previous = CASE WHEN ? = 'trash' AND folder = 'snoozed' THEN 'inbox'
				WHEN ? = 'trash' AND folder != 'trash' THEN folder ELSE previous END,
			origin = CASE WHEN ? = 'inbox' THEN 'inbox' ELSE origin END,
			folder = CASE WHEN ? = 'restore' AND folder = 'trash' THEN coalesce(nullif(previous,''),origin)
				WHEN ? = 'restore' AND folder = 'archive' THEN origin
				WHEN ? <> 'restore' THEN ? ELSE folder END,
			seen=coalesce(?,seen),star=coalesce(?,star) WHERE owner=? AND id=?`,
			change.Folder, change.Folder, change.Folder, change.Folder, change.Folder, change.Folder, change.Folder,
			change.Read, change.Star, owner, id)
		if err != nil {
			return err
		}
		if count, err := result.RowsAffected(); err != nil {
			return err
		} else if count == 0 {
			return sql.ErrNoRows
		}
		if change.Folder != nil {
			if _, err := tx.ExecContext(ctx, `DELETE FROM mail_snoozes WHERE letter_id=?
				AND EXISTS (SELECT 1 FROM letters WHERE id=? AND folder<>'snoozed')`, id, id); err != nil {
				return err
			}
			if err := invalidateMovedUID(ctx, tx, id); err != nil {
				return err
			}
		}
	}
	return a.commitMailbox(tx, owner)
}
