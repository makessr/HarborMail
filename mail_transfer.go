package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const transferLimit = 64 << 20

var (
	transferSlots     = make(chan struct{}, 2)
	errTransferFolder = errors.New("invalid transfer folder")
	errTransferSize   = errors.New("mail transfer limit exceeded")
)

func transferSlot(w http.ResponseWriter) bool {
	select {
	case transferSlots <- struct{}{}:
		return true
	default:
		fail(w, http.StatusTooManyRequests, "正在处理其他邮件迁移，请稍后重试")
		return false
	}
}

func transferFolder(folder string, importing bool) bool {
	switch folder {
	case "inbox", "sent", "archive":
		return true
	case "drafts", "trash", "junk", "snoozed":
		return !importing
	default:
		return customFolderID.MatchString(folder)
	}
}

func transferError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case isQuotaExceeded(err):
		fail(w, http.StatusInsufficientStorage, "邮箱存储已达到硬配额，未导入这批邮件")
	case errors.Is(err, errTransferFolder), errors.Is(err, sql.ErrNoRows):
		fail(w, http.StatusNotFound, "找不到指定文件夹或部分所选邮件")
	case errors.Is(err, errTransferSize):
		fail(w, http.StatusRequestEntityTooLarge, "每批最多 100 封、合计 64 MiB，请分批处理")
	default:
		log.Print("mail transfer: ", err)
		fail(w, http.StatusInternalServerError, "邮件迁移失败，请稍后重试")
	}
	return true
}

type invalidEML int

func (e invalidEML) Error() string {
	return fmt.Sprintf("第 %d 个文件不是有效的 EML 邮件", e)
}

func importMessage(raw []byte, owner string) (letter, bool) {
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil || len(parsed.Header) == 0 {
		return letter{}, false
	}
	if parsed.Header.Get("From") == "" && parsed.Header.Get("To") == "" && parsed.Header.Get("Subject") == "" &&
		parsed.Header.Get("Message-ID") == "" && parsed.Header.Get("Date") == "" {
		return letter{}, false
	}
	m := interpret(raw, "", owner)
	if recipients, err := parsed.Header.AddressList("To"); err == nil && len(recipients) > 0 {
		if recipient, err := address(recipients[0].Address); err == nil {
			m.To = recipient
		}
	}
	if date, err := parsed.Header.Date(); err == nil && date.Year() >= 1 && date.Year() <= 9999 {
		m.Date = date.Unix()
	}
	return m, true
}

func readImportFile(header *multipart.FileHeader) ([]byte, error) {
	file, err := header.Open()
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, mailLimit+1))
}

func (a *app) importMessages(ctx context.Context, owner, folder string, files []*multipart.FileHeader) error {
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
	if err := verifyFolder(ctx, tx, owner, folder); err != nil {
		return err
	}
	origin := "inbox"
	if folder == "sent" {
		origin = "sent"
	}
	for i, file := range files {
		raw, err := readImportFile(file)
		if err != nil {
			return err
		}
		if len(raw) > mailLimit {
			return errTransferSize
		}
		m, valid := importMessage(raw, owner)
		if !valid {
			return invalidEML(i + 1)
		}
		if _, err := insertLetter(tx, owner, m, folder, origin, raw); err != nil {
			return err
		}
	}
	return a.commitMailbox(tx, owner)
}

func (a *app) importMail(w http.ResponseWriter, r *http.Request, owner string) {
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "multipart/form-data" {
		fail(w, http.StatusUnsupportedMediaType, "请上传 EML 文件")
		return
	}
	if !transferSlot(w) {
		return
	}
	defer func() { <-transferSlots }()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(2 * time.Minute))
	_ = controller.SetWriteDeadline(time.Now().Add(3 * time.Minute))
	r.Body = http.MaxBytesReader(w, r.Body, transferLimit+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || errors.Is(err, multipart.ErrMessageTooLarge) {
			transferError(w, errTransferSize)
		} else {
			fail(w, http.StatusBadRequest, "无法读取上传的文件")
		}
		return
	}
	defer r.MultipartForm.RemoveAll()
	folder := r.PostForm.Get("folder")
	if !transferFolder(folder, true) {
		transferError(w, errTransferFolder)
		return
	}
	files := r.MultipartForm.File["messages"]
	if len(files) < 1 || len(files) > maxBatchLetters {
		fail(w, http.StatusBadRequest, "请选择 1–100 个 EML 文件")
		return
	}
	var total int64
	for i, file := range files {
		if !strings.EqualFold(filepath.Ext(file.Filename), ".eml") || file.Size == 0 {
			fail(w, http.StatusBadRequest, invalidEML(i+1).Error())
			return
		}
		total += file.Size
		if file.Size > mailLimit {
			fail(w, http.StatusRequestEntityTooLarge, "单封 EML 邮件最多 10 MiB")
			return
		}
	}
	if total > transferLimit {
		transferError(w, errTransferSize)
		return
	}
	err := a.importMessages(r.Context(), owner, folder, files)
	var invalid invalidEML
	if errors.As(err, &invalid) {
		fail(w, http.StatusBadRequest, invalid.Error())
		return
	}
	if !transferError(w, err) {
		reply(w, http.StatusCreated, map[string]int{"imported": len(files)})
	}
}

type exportMailRequest struct {
	Folder string  `json:"folder"`
	IDs    []int64 `json:"ids"`
}

func (input *exportMailRequest) valid() bool {
	if input.Folder != "" {
		return len(input.IDs) == 0 && transferFolder(input.Folder, false)
	}
	if len(input.IDs) == 0 || len(input.IDs) > maxBatchLetters {
		return false
	}
	slices.Sort(input.IDs)
	for i, id := range input.IDs {
		if id <= 0 || (i > 0 && id == input.IDs[i-1]) {
			return false
		}
	}
	return true
}

func exportIDs(ctx context.Context, tx *sql.Tx, owner string, input exportMailRequest) ([]int64, error) {
	query := `SELECT id,length(raw) FROM letters WHERE owner=? AND `
	args := []any{owner}
	if input.Folder != "" {
		if err := verifyFolder(ctx, tx, owner, input.Folder); err != nil {
			return nil, err
		}
		query += `folder=?`
		args = append(args, input.Folder)
	} else {
		query += `id IN (` + placeholders(len(input.IDs)) + `)`
		for _, id := range input.IDs {
			args = append(args, id)
		}
	}
	rows, err := tx.QueryContext(ctx, query+` ORDER BY id LIMIT ?`, append(args, maxBatchLetters+1)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	var total int64
	for rows.Next() {
		var id, size int64
		if err := rows.Scan(&id, &size); err != nil {
			return nil, err
		}
		total += size
		if total > transferLimit || len(ids) >= maxBatchLetters {
			return nil, errTransferSize
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if input.Folder == "" && len(ids) != len(input.IDs) {
		return nil, sql.ErrNoRows
	}
	return ids, nil
}

func (a *app) buildMailArchive(ctx context.Context, owner string, input exportMailRequest, destination io.Writer) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ids, err := exportIDs(ctx, tx, owner, input)
	if err != nil {
		return err
	}
	archive := zip.NewWriter(destination)
	defer archive.Close()
	var total int64
	for _, id := range ids {
		var raw []byte
		if err := tx.QueryRowContext(ctx, `SELECT raw FROM letters WHERE owner=? AND id=?`, owner, id).Scan(&raw); err != nil {
			return err
		}
		total += int64(len(raw))
		if total > transferLimit {
			return errTransferSize
		}
		member, err := archive.Create("mail-" + strconv.FormatInt(id, 10) + ".eml")
		if err != nil {
			return err
		}
		if _, err := member.Write(raw); err != nil {
			return err
		}
	}
	if err := archive.Close(); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *app) exportMail(w http.ResponseWriter, r *http.Request, owner string) {
	var input exportMailRequest
	if !decode(w, r, &input) {
		return
	}
	if !input.valid() {
		fail(w, http.StatusBadRequest, "请选择一个文件夹或 1–100 封邮件")
		return
	}
	if !transferSlot(w) {
		return
	}
	defer func() { <-transferSlots }()
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(2 * time.Minute))
	file, err := os.CreateTemp("", "harbor-mail-*.zip")
	if transferError(w, err) {
		return
	}
	defer func() {
		file.Close()
		os.Remove(file.Name())
	}()
	if transferError(w, a.buildMailArchive(r.Context(), owner, input, file)) {
		return
	}
	size, err := file.Seek(0, io.SeekCurrent)
	if transferError(w, err) {
		return
	}
	if _, err := file.Seek(0, io.SeekStart); transferError(w, err) {
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="harbor-mail.zip"`)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	_, _ = io.Copy(w, file)
}
