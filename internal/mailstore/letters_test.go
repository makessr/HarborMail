package mailstore

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	_ "modernc.org/sqlite"
)

func TestInsertLetter(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `CREATE TABLE letters (
		id INTEGER PRIMARY KEY, owner TEXT NOT NULL CHECK(owner<>''), sender TEXT, recipient TEXT, subject TEXT,
		body TEXT, stamp INTEGER, folder TEXT, origin TEXT, seen BOOLEAN, star BOOLEAN,
		raw BLOB, detail_meta BLOB, pop_octets INTEGER
	)`)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	expected := LetterRow{
		Owner: "owner@example.test", Sender: "sender@example.test", Recipient: "alias@example.test",
		Subject: "主题", Body: "内容", Stamp: 123, Folder: "inbox", Origin: "smtp",
		Seen: true, Raw: []byte{0, 255, 10}, Detail: []byte(`{"attachments":1}`), POPOctets: 5,
	}
	id, err := InsertLetter(ctx, tx, expected)
	if err != nil || id <= 0 {
		t.Fatalf("insert returned id=%d, err=%v", id, err)
	}
	var stored LetterRow
	err = tx.QueryRowContext(ctx, `SELECT owner,sender,recipient,subject,body,stamp,folder,origin,seen,star,
		raw,detail_meta,pop_octets FROM letters WHERE id=?`, id).Scan(&stored.Owner, &stored.Sender, &stored.Recipient,
		&stored.Subject, &stored.Body, &stored.Stamp, &stored.Folder, &stored.Origin,
		&stored.Seen, &stored.Star, &stored.Raw, &stored.Detail, &stored.POPOctets)
	if err != nil || !reflect.DeepEqual(stored, expected) {
		t.Fatalf("letter round trip failed: got %+v, err=%v", stored, err)
	}
	if id, err := InsertLetter(ctx, tx, LetterRow{}); id != 0 || err == nil {
		t.Fatalf("constraint error was not returned: id=%d, err=%v", id, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM letters").Scan(&count); err != nil || count != 0 {
		t.Fatalf("caller rollback did not remove the row: count=%d, err=%v", count, err)
	}
}
