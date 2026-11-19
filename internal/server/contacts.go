package server

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type contact struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Note  string `json:"note"`
}

func uniqueConflict(err error) bool {
	var dbError *mysql.MySQLError
	var sqliteError *sqlite.Error
	return (errors.As(err, &dbError) && dbError.Number == 1062) ||
		(errors.As(err, &sqliteError) && sqliteError.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE)
}

func contactHTTPError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, sql.ErrNoRows):
		fail(w, http.StatusNotFound, "找不到该联系人")
	case uniqueConflict(err):
		fail(w, http.StatusConflict, "此邮箱地址已在通讯录")
	default:
		log.Print("contact operation: ", err)
		fail(w, http.StatusInternalServerError, "联系人操作失败")
	}
	return true
}

func (a *app) listContacts(w http.ResponseWriter, r *http.Request, owner string) {
	params := r.URL.Query()
	limit := 30
	if value := params.Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 50 {
			fail(w, http.StatusBadRequest, "联系人每页应为 1–50 个")
			return
		}
		limit = parsed
	}
	query := `SELECT id,name,email,note FROM contacts WHERE owner=?`
	args := []any{owner}
	if email := params.Get("email"); email != "" {
		canonical, err := address(email)
		if err != nil {
			fail(w, http.StatusBadRequest, "请输入有效的邮箱地址")
			return
		}
		query += ` AND email=?`
		args = append(args, canonical)
	}
	if search := strings.ToLower(strings.TrimSpace(params.Get("q"))); search != "" {
		query += ` AND instr(search_key,?)>0`
		args = append(args, search)
	}
	if before, _ := strconv.ParseInt(params.Get("before"), 10, 64); before > 0 {
		query += ` AND id<?`
		args = append(args, before)
	}
	rows, err := a.db.QueryContext(r.Context(), query+` ORDER BY id DESC LIMIT ?`, append(args, limit+1)...)
	if contactHTTPError(w, err) {
		return
	}
	defer rows.Close()
	items := make([]contact, 0, limit+1)
	for rows.Next() {
		var item contact
		if err := rows.Scan(&item.ID, &item.Name, &item.Email, &item.Note); contactHTTPError(w, err) {
			return
		}
		items = append(items, item)
	}
	if contactHTTPError(w, rows.Err()) {
		return
	}
	var next int64
	if len(items) > limit {
		items = items[:limit]
		next = items[limit-1].ID
	}
	reply(w, http.StatusOK, map[string]any{"contacts": items, "next": next})
}

func (a *app) saveContact(w http.ResponseWriter, r *http.Request, owner string) {
	var item contact
	if !decode(w, r, &item) {
		return
	}
	email := strings.TrimSpace(item.Email)
	canonical, err := address(email)
	if err != nil || !strings.EqualFold(email, canonical) {
		fail(w, http.StatusBadRequest, "请输入有效的邮箱地址")
		return
	}
	target, err := a.destination(r.Context(), a.db, canonical)
	if contactHTTPError(w, err) {
		return
	}
	item.Email = target.address
	item.Name, item.Note = strings.TrimSpace(item.Name), strings.TrimSpace(item.Note)
	if utf8.RuneCountInString(item.Name) > 100 || utf8.RuneCountInString(item.Note) > 500 ||
		strings.ContainsFunc(item.Name, unicode.IsControl) {
		fail(w, http.StatusBadRequest, "姓名须在 100 字内，备注须在 500 字内")
		return
	}
	searchKey := strings.ToLower(item.Name + "\n" + item.Email)
	ctx := r.Context()
	if r.Method == http.MethodPost {
		result, err := a.db.ExecContext(ctx, `INSERT INTO contacts (owner,email,name,note,search_key) VALUES (?,?,?,?,?)`,
			owner, item.Email, item.Name, item.Note, searchKey)
		if contactHTTPError(w, err) {
			return
		}
		item.ID, err = result.LastInsertId()
		if !contactHTTPError(w, err) {
			reply(w, http.StatusCreated, item)
		}
		return
	}
	item.ID = letterID(r)
	result, err := a.db.ExecContext(ctx, `UPDATE contacts SET email=?,name=?,note=?,search_key=? WHERE owner=? AND id=?`,
		item.Email, item.Name, item.Note, searchKey, owner, item.ID)
	if !contactHTTPError(w, requireAffectedRow(result, err)) {
		reply(w, http.StatusOK, item)
	}
}

func (a *app) deleteContact(w http.ResponseWriter, r *http.Request, owner string) {
	result, err := a.db.ExecContext(r.Context(), `DELETE FROM contacts WHERE owner=? AND id=?`, owner, letterID(r))
	if !contactHTTPError(w, requireAffectedRow(result, err)) {
		w.WriteHeader(http.StatusNoContent)
	}
}
