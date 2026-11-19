package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var errSearchConflict = errors.New("saved search revision changed")

type savedSearchFilters struct {
	Sender    string `json:"sender,omitempty"`
	Recipient string `json:"recipient,omitempty"`
	Seen      string `json:"seen,omitempty"`
	Start     string `json:"start,omitempty"`
	End       string `json:"end,omitempty"`
	Content   string `json:"content,omitempty"`
}

type savedSearch struct {
	ID          int64              `json:"id"`
	Revision    int64              `json:"revision"`
	Name        string             `json:"name"`
	Folder      string             `json:"folder"`
	Label       int64              `json:"label"`
	Query       string             `json:"query"`
	Filters     savedSearchFilters `json:"filters"`
	Unavailable bool               `json:"unavailable,omitempty"`
}

func (item *savedSearch) normalize() string {
	item.Name, item.Query = strings.TrimSpace(item.Name), strings.TrimSpace(item.Query)
	if item.Name == "" || utf8.RuneCountInString(item.Name) > 40 || strings.ContainsFunc(item.Name, unicode.IsControl) {
		return "搜索名称须为 1–40 个字符"
	}
	filters := &item.Filters
	filters.Sender, filters.Recipient = strings.TrimSpace(filters.Sender), strings.TrimSpace(filters.Recipient)
	if utf8.RuneCountInString(item.Query) > 512 || utf8.RuneCountInString(filters.Sender) > 254 || utf8.RuneCountInString(filters.Recipient) > 254 ||
		strings.ContainsFunc(item.Query+filters.Sender+filters.Recipient, unicode.IsControl) {
		return "关键词、发件人或收件地址超出保存限制"
	}
	switch filters.Seen {
	case "", "read", "unread":
	default:
		return "请选择有效的阅读状态"
	}
	if filters.Content != "" && filters.Content != "1" {
		return "请选择有效的搜索字段"
	}
	for _, value := range []string{filters.Start, filters.End} {
		if value != "" {
			if date, err := time.Parse(time.DateOnly, value); err != nil || date.Format(time.DateOnly) != value || date.Year() < 1 {
				return "请选择有效的搜索日期"
			}
		}
	}
	if filters.Start != "" && filters.End != "" && filters.Start > filters.End {
		return "结束日期不能早于开始日期"
	}
	switch item.Folder {
	case "inbox", "all", "starred", "drafts", "sent", "archive", "junk", "trash", "snoozed":
	case "outbox":
		if *filters != (savedSearchFilters{}) {
			return "发件箱搜索可保存关键词"
		}
	default:
		if !customFolderID.MatchString(item.Folder) {
			return "请选择有效的搜索范围"
		}
	}
	if item.Label < 0 || (item.Label != 0 && item.Folder != "all") {
		return "请选择有效的搜索范围"
	}
	return ""
}

func savedSearchError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, sql.ErrNoRows):
		fail(w, 404, "找不到此搜索或所选范围")
	case errors.Is(err, errSearchConflict):
		fail(w, 409, "搜索已在其他窗口修改，请重新打开后再保存")
	case uniqueConflict(err):
		fail(w, 409, "已有同名的搜索")
	default:
		log.Print("saved search: ", err)
		fail(w, 500, "保存的搜索操作失败")
	}
	return true
}

func (a *app) listSavedSearches(w http.ResponseWriter, r *http.Request, owner string) {
	rows, err := a.db.QueryContext(r.Context(), `SELECT s.id,s.revision,s.name,s.folder,s.label_id,s.keyword,s.filters,
		f.folder,l.id FROM saved_searches s LEFT JOIN custom_folders f ON f.owner=s.owner AND f.folder=s.folder
		LEFT JOIN mail_labels l ON l.owner=s.owner AND l.id=s.label_id WHERE s.owner=? ORDER BY s.name_key,s.id`, owner)
	if savedSearchError(w, err) {
		return
	}
	defer rows.Close()
	items := make([]savedSearch, 0)
	for rows.Next() {
		var item savedSearch
		var filters []byte
		var folder sql.NullString
		var label sql.NullInt64
		if err := rows.Scan(&item.ID, &item.Revision, &item.Name, &item.Folder, &item.Label, &item.Query,
			&filters, &folder, &label); savedSearchError(w, err) {
			return
		}
		if savedSearchError(w, json.Unmarshal(filters, &item.Filters)) {
			return
		}
		item.Unavailable = (customFolderID.MatchString(item.Folder) && !folder.Valid) || (item.Label != 0 && !label.Valid)
		items = append(items, item)
	}
	if !savedSearchError(w, rows.Err()) {
		reply(w, 200, items)
	}
}

func checkSearchRevision(ctx context.Context, q folderQueryer, owner string, id int64, result sql.Result, err error) error {
	err = requireAffectedRow(result, err)
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var current int64
	if err := q.QueryRowContext(ctx, `SELECT revision FROM saved_searches WHERE owner=? AND id=?`, owner, id).Scan(&current); err != nil {
		return err
	}
	return errSearchConflict
}

func (a *app) saveSearch(w http.ResponseWriter, r *http.Request, owner string) {
	var item savedSearch
	if !decode(w, r, &item) {
		return
	}
	if invalid := item.normalize(); invalid != "" {
		fail(w, 400, invalid)
		return
	}
	item.ID = letterID(r)
	edit := r.Method == http.MethodPut
	if edit && (item.ID == 0 || item.Revision < 1) {
		fail(w, 400, "请选择要更新的搜索版本")
		return
	}
	filters, err := json.Marshal(item.Filters)
	if savedSearchError(w, err) {
		return
	}
	ctx := r.Context()
	tx, err := a.db.BeginTx(ctx, nil)
	if savedSearchError(w, err) {
		return
	}
	defer tx.Rollback()
	// Serialize scope validation with deletion of a folder or label.
	if savedSearchError(w, lockIMAPUIDs(ctx, tx)) || savedSearchError(w, verifyFolder(ctx, tx, owner, item.Folder)) {
		return
	}
	if item.Label != 0 {
		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM mail_labels WHERE owner=? AND id=?`, owner, item.Label).Scan(&id); savedSearchError(w, err) {
			return
		}
	}
	nameKey := strings.ToLower(item.Name)
	status := 201
	if edit {
		result, err := tx.ExecContext(ctx, `UPDATE saved_searches SET revision=revision+1,name=?,name_key=?,folder=?,
			label_id=?,keyword=?,filters=? WHERE owner=? AND id=? AND revision=?`, item.Name, nameKey, item.Folder,
			item.Label, item.Query, filters, owner, item.ID, item.Revision)
		if savedSearchError(w, checkSearchRevision(ctx, tx, owner, item.ID, result, err)) {
			return
		}
		item.Revision++
		status = 200
	} else {
		result, err := tx.ExecContext(ctx, `INSERT INTO saved_searches(owner,revision,name,name_key,folder,label_id,keyword,filters)
			VALUES(?,1,?,?,?,?,?,?)`, owner, item.Name, nameKey, item.Folder, item.Label, item.Query, filters)
		if savedSearchError(w, err) {
			return
		}
		if item.ID, err = result.LastInsertId(); savedSearchError(w, err) {
			return
		}
		item.Revision = 1
	}
	item.Unavailable = false
	if !savedSearchError(w, a.commitMailbox(tx, owner)) {
		reply(w, status, item)
	}
}

func (a *app) deleteSavedSearch(w http.ResponseWriter, r *http.Request, owner string) {
	revision, _ := strconv.ParseInt(strings.Trim(r.Header.Get("If-Match"), `"`), 10, 64)
	if revision < 1 {
		fail(w, 428, "请重新打开后再删除搜索")
		return
	}
	ctx, id := r.Context(), letterID(r)
	result, err := a.db.ExecContext(ctx, `DELETE FROM saved_searches WHERE owner=? AND id=? AND revision=?`, owner, id, revision)
	if !savedSearchError(w, checkSearchRevision(ctx, a.db, owner, id, result, err)) {
		a.events.notify(owner)
		w.WriteHeader(204)
	}
}
