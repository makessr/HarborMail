package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
)

func TestLegacyMailDetailMatchesCachedResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := openDB("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	const owner = "owner@harbor.test"
	m := letter{From: "sender@harbor.test", To: owner, Body: "test"}
	raw, err := encodeLetter(m, "harbor.test", []uploadedAttachment{{name: "notes.txt", data: []byte("attachment")}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := insertLetter(t.Context(), tx, owner, m, "inbox", "inbox", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	a := &app{db: db}
	r := httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("id", strconv.FormatInt(id, 10))
	loaded := httptest.NewRecorder()
	a.read(loaded, r, owner)
	if _, err := db.Exec(`ALTER TABLE letters DROP COLUMN detail_meta`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openDB("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a.db = db
	legacy := httptest.NewRecorder()
	a.read(legacy, r, owner)
	if legacy.Code != http.StatusOK || loaded.Code != http.StatusOK || !bytes.Equal(loaded.Body.Bytes(), legacy.Body.Bytes()) {
		t.Fatalf("legacy and indexed details differ: %d vs %d", legacy.Code, loaded.Code)
	}
	var metadata []byte
	if err := db.QueryRow(`SELECT detail_meta FROM letters WHERE id=?`, id).Scan(&metadata); err != nil || len(metadata) == 0 {
		t.Fatalf("old row was not backfilled: %v", err)
	}
}

func BenchmarkReadMailDetail(b *testing.B) {
	db, err := openDB("sqlite", filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	const owner = "reader@harbor.test"
	m := letter{From: "sender@harbor.test", To: owner, Body: "Benchmark"}
	raw, err := encodeLetter(m, "harbor.test", []uploadedAttachment{{name: "sample.bin", data: make([]byte, 4<<20)}})
	if err != nil {
		b.Fatal(err)
	}
	result, err := db.Exec(`INSERT INTO letters(owner,sender,recipient,subject,body,stamp,folder,origin,raw,detail_meta)
		VALUES(?,?,?,?,?,0,'inbox','inbox',?,?)`, owner, m.From, m.To, m.Subject, m.Body, raw, encodeMailDetail(raw))
	if err != nil {
		b.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		b.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("id", strconv.FormatInt(id, 10))
	a := &app{db: db, recoveryMode: true}
	run := func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			w := httptest.NewRecorder()
			a.read(w, r, owner)
			if w.Code != http.StatusOK {
				b.Fatal(w.Code)
			}
		}
	}
	b.Run("stored", run)
	if _, err := db.Exec(`UPDATE letters SET detail_meta=NULL WHERE id=?`, id); err != nil {
		b.Fatal(err)
	}
	b.Run("raw", run)
}
