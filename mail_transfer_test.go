package main

import (
	"archive/zip"
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEMLTransferPreservesBytesAndRollsBackAnInvalidBatch(t *testing.T) {
	const owner = "owner@harbor.test"
	const foreign = "other@harbor.test"
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db}
	importBatch := func(account, folder string, messages ...string) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		if err := form.WriteField("folder", folder); err != nil {
			t.Fatal(err)
		}
		for _, raw := range messages {
			part, err := form.CreateFormFile("messages", "mail.eml")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(part, raw); err != nil {
				t.Fatal(err)
			}
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/mail-import", &body)
		r.Header.Set("Content-Type", form.FormDataContentType())
		w := httptest.NewRecorder()
		a.importMail(w, r, account)
		return w
	}
	const raw = "From: someone@outside.test\r\nTo: archived@old.test\r\nDate: Tue, 02 Jan 2024 15:04:05 +0000\r\n" +
		"Subject: Imported message\r\n\r\nOriginal body.\r\n"
	if r := importBatch(owner, "archive", raw, "this is not an email"); r.Code != http.StatusBadRequest {
		t.Fatalf("invalid batch returned %d: %s", r.Code, r.Body)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM letters`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed import wrote %d messages: %v", count, err)
	}
	if r := importBatch(owner, "archive", raw); r.Code != http.StatusCreated {
		t.Fatalf("valid batch returned %d: %s", r.Code, r.Body)
	}
	var id, stamp int64
	var recipient string
	if err := db.QueryRow(`SELECT id,stamp,recipient FROM letters WHERE owner=? AND folder='archive'`, owner).
		Scan(&id, &stamp, &recipient); err != nil || stamp != time.Date(2024, 1, 2, 15, 4, 5, 0, time.UTC).Unix() ||
		recipient != "archived@old.test" {
		t.Fatalf("import metadata: id=%d stamp=%d recipient=%q err=%v", id, stamp, recipient, err)
	}
	export := func(account, input string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/mail-export", strings.NewReader(input))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		a.exportMail(w, r, account)
		return w
	}
	if result := export(foreign, `{"ids":[`+strconv.FormatInt(id, 10)+`]}`); result.Code != http.StatusNotFound {
		t.Fatalf("non-owner export returned %d", result.Code)
	}
	result := export(owner, `{"folder":"archive"}`)
	if result.Code != http.StatusOK || result.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("folder export returned %d", result.Code)
	}
	archive, err := zip.NewReader(bytes.NewReader(result.Body.Bytes()), int64(result.Body.Len()))
	if err != nil || len(archive.File) != 1 {
		t.Fatalf("unexpected archive: %v", err)
	}
	entry, err := archive.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer entry.Close()
	stored, err := io.ReadAll(entry)
	if err != nil || string(stored) != raw {
		t.Fatalf("export changed raw bytes: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for range maxBatchLetters {
		if _, err := tx.Exec(`INSERT INTO letters (owner,sender,recipient,subject,body,stamp,folder,origin,raw)
			SELECT owner,sender,recipient,subject,body,stamp,folder,origin,raw FROM letters WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if r := export(owner, `{"folder":"archive"}`); r.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized folder exported: HTTP %d", r.Code)
	}
	if r := export(owner, `{"ids":[`+strconv.FormatInt(id, 10)+`]}`); r.Code != http.StatusOK {
		t.Fatalf("could not export a small selection from an oversized folder: HTTP %d", r.Code)
	}
}
