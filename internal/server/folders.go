package server

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	errFolderName        = errors.New("Invalid folder name")
	errFolderExists      = errors.New("A folder with that name already exists")
	errFolderNotEmpty    = errors.New("Move all messages out before deleting this folder")
	errFolderHasChildren = errors.New("Move or delete the child folders first")
	errFolderIntoChild   = errors.New("Cannot move a folder inside itself")
	errFolderUsedByRule  = errors.New("Remove mail rule references before deleting this folder")
	customFolderID       = regexp.MustCompile(`^u[0-9a-f]{15}$`)
)

type customFolder struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Subscribed bool   `json:"-"`
}

type folderQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func renameLegacyJunkFolders(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT owner,folder,label FROM custom_folders
		WHERE lookup IN ('junk','垃圾邮件') ORDER BY owner,folder`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type conflict struct{ owner, id, label string }
	var conflicts []conflict
	for rows.Next() {
		var folder conflict
		if err := rows.Scan(&folder.owner, &folder.id, &folder.label); err != nil {
			return err
		}
		conflicts = append(conflicts, folder)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, folder := range conflicts {
		prefix := folder.label + " (自定义"
		label := prefix + ")"
		for number := 2; ; number++ {
			lookup := strings.ToLower(label)
			var exists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM custom_folders WHERE owner=? AND lookup=?)`,
				folder.owner, lookup).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				if _, err := tx.ExecContext(ctx, `UPDATE custom_folders SET label=?,lookup=? WHERE owner=? AND folder=?`,
					label, lookup, folder.owner, folder.id); err != nil {
					return err
				}
				break
			}
			label = prefix + " " + strconv.Itoa(number) + ")"
		}
	}
	return tx.Commit()
}

func folderName(input string) (string, error) {
	label := strings.TrimSpace(input)
	if !utf8.ValidString(label) || strings.ContainsAny(label, "\\*%") || strings.ContainsFunc(label, unicode.IsControl) {
		return "", errFolderName
	}
	parts := strings.Split(label, "/")
	for i, part := range parts {
		parts[i] = strings.TrimSpace(part)
		if parts[i] == "" {
			return "", errFolderName
		}
	}
	for _, fixed := range imapFolders {
		if strings.EqualFold(parts[0], fixed.name) {
			if len(parts) == 1 {
				return "", errFolderExists
			}
			parts[0] = fixed.name
			break
		}
	}
	switch parts[0] {
	case "收件箱", "已发送", "星标", "草稿箱", "发件箱", "归档", "垃圾邮件", "废纸篓":
		return "", errFolderExists
	}
	label = strings.Join(parts, "/")
	if utf8.RuneCountInString(label) > 64 {
		return "", errFolderName
	}
	return label, nil
}

func parentFolderPath(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}

func readCustomFolder(ctx context.Context, q folderQueryer, owner, id string) (customFolder, error) {
	var result customFolder
	err := q.QueryRowContext(ctx, `SELECT folder,label FROM custom_folders WHERE owner=? AND folder=?`, owner, id).
		Scan(&result.ID, &result.Label)
	return result, err
}

func verifyFolder(ctx context.Context, q folderQueryer, owner, id string) error {
	if !customFolderID.MatchString(id) {
		return nil
	}
	_, err := readCustomFolder(ctx, q, owner, id)
	return err
}

func listCustomFolders(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, owner string) ([]customFolder, error) {
	rows, err := q.QueryContext(ctx, `SELECT folder,label,subscribed FROM custom_folders WHERE owner=? ORDER BY lookup,folder`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]customFolder, 0)
	for rows.Next() {
		var entry customFolder
		if err := rows.Scan(&entry.ID, &entry.Label, &entry.Subscribed); err != nil {
			return nil, err
		}
		result = append(result, entry)
	}
	return result, rows.Err()
}

func (a *app) saveCustomFolder(ctx context.Context, owner, id, name string) (customFolder, error) {
	label, err := folderName(name)
	if err != nil {
		return customFolder{}, err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return customFolder{}, err
	}
	defer tx.Rollback()
	// Folder names and parent creation serialize per mailbox. Folder IDs are stable.
	if err := a.requireActiveAccount(ctx, tx, owner); err != nil {
		return customFolder{}, err
	}
	folders, err := listCustomFolders(ctx, tx, owner)
	if err != nil {
		return customFolder{}, err
	}
	byPath := make(map[string]customFolder, len(folders)+len(imapFolders))
	for _, fixed := range imapFolders {
		byPath[strings.ToLower(fixed.name)] = customFolder{Label: fixed.name}
	}
	var original customFolder
	for _, folder := range folders {
		byPath[strings.ToLower(folder.Label)] = folder
		if folder.ID == id {
			original = folder
		}
	}
	lookup := strings.ToLower(label)
	if id != "" {
		if original.ID == "" {
			return customFolder{}, sql.ErrNoRows
		}
		if strings.HasPrefix(lookup, strings.ToLower(original.Label)+"/") {
			return customFolder{}, errFolderIntoChild
		}
	}
	if existing, ok := byPath[lookup]; ok && (id == "" || existing.ID != id) {
		return customFolder{}, errFolderExists
	}
	label, err = ensureFolderParents(ctx, tx, owner, label, byPath)
	if err != nil {
		return customFolder{}, err
	}
	if id == "" {
		id = "u" + token()[:15]
		_, err = tx.ExecContext(ctx, `INSERT INTO custom_folders (folder,owner,label,lookup) VALUES (?,?,?,?)`,
			id, owner, label, strings.ToLower(label))
	} else {
		err = renameFolderTree(ctx, tx, owner, original, label, folders, byPath)
	}
	if err != nil {
		return customFolder{}, err
	}
	if err := a.commitMailbox(tx, owner); err != nil {
		return customFolder{}, err
	}
	return customFolder{ID: id, Label: label}, nil
}

func ensureFolderParents(ctx context.Context, tx *sql.Tx, owner, label string, byPath map[string]customFolder) (string, error) {
	parts := strings.Split(label, "/")
	prefix := ""
	for _, part := range parts[:len(parts)-1] {
		prefix += part
		lookup := strings.ToLower(prefix)
		if parent, found := byPath[lookup]; found {
			prefix = parent.Label
		} else {
			parent := customFolder{ID: "u" + token()[:15], Label: prefix}
			if _, err := tx.ExecContext(ctx, `INSERT INTO custom_folders(folder,owner,label,lookup,subscribed)
				VALUES(?,?,?,?,FALSE)`, parent.ID, owner, prefix, lookup); err != nil {
				return "", err
			}
			byPath[lookup] = parent
		}
		prefix += "/"
	}
	return prefix + parts[len(parts)-1], nil
}

func renameFolderTree(ctx context.Context, tx *sql.Tx, owner string, original customFolder, label string, folders []customFolder, byPath map[string]customFolder) error {
	oldPath := strings.ToLower(original.Label)
	depth := strings.Count(original.Label, "/") + 1
	for _, folder := range folders {
		current := strings.ToLower(folder.Label)
		next := label
		if folder.ID != original.ID {
			if !strings.HasPrefix(current, oldPath+"/") {
				continue
			}
			next += "/" + strings.Join(strings.Split(folder.Label, "/")[depth:], "/")
		}
		if utf8.RuneCountInString(next) > 64 {
			return errFolderName
		}
		lookup := strings.ToLower(next)
		if other, found := byPath[lookup]; found && other.ID != folder.ID {
			return errFolderExists
		}
		if _, err := tx.ExecContext(ctx, `UPDATE custom_folders SET label=?,lookup=? WHERE owner=? AND folder=?`, next, lookup, owner, folder.ID); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) deleteCustomFolder(ctx context.Context, owner, id string) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return err
	}
	if err := a.requireActiveAccount(ctx, tx, owner); err != nil {
		return err
	}
	folder, err := readCustomFolder(ctx, tx, owner, id)
	if err != nil {
		return err
	}
	var children bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM custom_folders
		WHERE owner=? AND instr(lookup,?)=1)`, owner, strings.ToLower(folder.Label)+"/").Scan(&children); err != nil {
		return err
	}
	if children {
		return errFolderHasChildren
	}
	var nonempty bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM letters WHERE owner=? AND folder=?)`, owner, id).
		Scan(&nonempty); err != nil {
		return err
	}
	if nonempty {
		return errFolderNotEmpty
	}
	var usedByRule bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM mail_rules WHERE owner=? AND rule_action='move' AND destination=?)`, owner, id).
		Scan(&usedByRule); err != nil {
		return err
	}
	if usedByRule {
		return errFolderUsedByRule
	}
	if _, err := tx.ExecContext(ctx, `UPDATE letters SET previous='' WHERE owner=? AND previous=?`, owner, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM custom_folders WHERE owner=? AND folder=?`, owner, id); err != nil {
		return err
	}
	return a.commitMailbox(tx, owner)
}

func folderHTTPError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, sql.ErrNoRows):
		fail(w, 404, "找不到此文件夹")
	case errors.Is(err, errFolderName):
		fail(w, 400, "文件夹路径须为 1–64 个字符。用 / 分隔层级，每级都要有名称，不能包含反斜杠、* 或 %")
	case errors.Is(err, errFolderExists):
		fail(w, 409, "此文件夹名称已被使用")
	case errors.Is(err, errFolderIntoChild):
		fail(w, 409, "不能将文件夹移到自己的子文件夹内")
	case errors.Is(err, errFolderHasChildren):
		fail(w, 409, "请先移动或删除子文件夹")
	case errors.Is(err, errFolderNotEmpty):
		fail(w, 409, "请先移出文件夹内的邮件")
	case errors.Is(err, errFolderUsedByRule):
		fail(w, 409, "请先修改或删除使用此文件夹的邮件规则")
	default:
		log.Print("folder operation: ", err)
		fail(w, 500, "文件夹操作失败")
	}
	return true
}

func (a *app) foldersHTTP(w http.ResponseWriter, r *http.Request, owner string) {
	result, err := listCustomFolders(r.Context(), a.db, owner)
	if !folderHTTPError(w, err) {
		reply(w, 200, result)
	}
}

func (a *app) saveFolderHTTP(w http.ResponseWriter, r *http.Request, owner string) {
	var input struct{ Label string }
	if !decode(w, r, &input) {
		return
	}
	entry, err := a.saveCustomFolder(r.Context(), owner, r.PathValue("folder"), input.Label)
	if folderHTTPError(w, err) {
		return
	}
	status := 200
	if r.Method == http.MethodPost {
		status = 201
	}
	reply(w, status, entry)
}

func (a *app) deleteFolderHTTP(w http.ResponseWriter, r *http.Request, owner string) {
	if !folderHTTPError(w, a.deleteCustomFolder(r.Context(), owner, r.PathValue("folder"))) {
		w.WriteHeader(204)
	}
}
