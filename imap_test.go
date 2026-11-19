package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/server"
	"golang.org/x/crypto/bcrypt"
)

func TestIMAPIdlePushesCommittedDelivery(t *testing.T) {
	const owner = "user@harbor.test"
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hash, err := bcrypt.GenerateFromPassword([]byte("test-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, accounts: map[string][]byte{owner: hash}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	service := server.New(imapBackend{app: a})
	service.AllowInsecureAuth = true
	enableHarborIMAPCommands(service)
	go func() { _ = service.Serve(listener) }()
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	waitFor := func(prefix string) {
		t.Helper()
		var transcript []string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("waiting for %q: %v, responses: %q", prefix, err, transcript)
			}
			transcript = append(transcript, line)
			if strings.HasPrefix(line, prefix) {
				return
			}
		}
	}
	send := func(command string) {
		t.Helper()
		if _, err := fmt.Fprint(conn, command+"\r\n"); err != nil {
			t.Fatal(err)
		}
	}
	waitFor("* OK")
	send("A1 LOGIN " + owner + " test-password")
	waitFor("A1 OK")
	send("A2 SELECT INBOX")
	waitFor("* 0 EXISTS")
	waitFor("A2 OK")
	send("A3 IDLE")
	waitFor("+ idling")
	session := &delivery{app: a, sender: "sender@harbor.test", recipients: []mailDestination{{address: owner, owner: owner}}}
	if err := session.Data(strings.NewReader("Subject: idle update\r\n\r\nbody\r\n")); err != nil {
		t.Fatal(err)
	}
	waitFor("* 1 EXISTS")
	send("DONE")
	waitFor("A3 OK")
	send("A4 NOOP")
	waitFor("A4 OK")
}

func TestIMAPCopyAppendsAndRollsBackOnStaleSource(t *testing.T) {
	const owner = "user@harbor.test"
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	raw := []byte("Subject: source\r\n\r\nbody\r\n")
	for _, folder := range []string{"archive", "inbox", "inbox"} {
		if err := insert(tx, owner, letter{From: "sender@harbor.test", To: owner}, folder, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	source := &imapMailbox{imapUser: imapUser{app: a, owner: owner}, folder: "inbox"}
	if _, err := source.Status(nil); err != nil {
		t.Fatal(err)
	}
	first, second := source.messages[0], source.messages[1]
	if _, err := db.Exec(`UPDATE letters SET folder='trash' WHERE id=?`, second.id); err != nil {
		t.Fatal(err)
	}
	both := new(imap.SeqSet)
	both.AddNum(first.Uid, second.Uid)
	if err := source.CopyMessages(true, both, "Archive"); !errors.Is(err, errIMAPMailboxChanged) {
		t.Fatalf("copy using a stale UID returned %v", err)
	}
	var copies int
	if err := db.QueryRow(`SELECT count(*) FROM letters WHERE owner=? AND folder='archive'`, owner).Scan(&copies); err != nil || copies != 1 {
		t.Fatalf("failed batch changed archive: count=%d error=%v", copies, err)
	}
	one := new(imap.SeqSet)
	one.AddNum(first.Uid)
	if err := source.CopyMessages(true, one, "Archive"); err != nil {
		t.Fatal(err)
	}
	archive, _, _, err := a.imapSnapshot(owner, "archive")
	if err != nil || len(archive) != 2 || archive[0].id >= first.id || archive[1].id <= second.id {
		t.Fatalf("copy did not append after older unassigned mail: %v (err=%v)", archive, err)
	}
	got, err := a.rawLetter(owner, archive[1].id)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("copied MIME bytes = %q (err=%v)", got, err)
	}
}

func TestIMAPUIDOnFolderMove(t *testing.T) {
	const owner = "user@harbor.test"
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	msg := letter{From: "sender@harbor.test", To: owner, Date: time.Now().Unix()}
	if err := insert(tx, owner, msg, "inbox", []byte("Subject: test\r\n\r\nbody\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	snapshot := func(folder string) ([]imapMessage, uint32) {
		t.Helper()
		messages, validity, _, err := a.imapSnapshot(owner, folder)
		if err != nil {
			t.Fatal(err)
		}
		return messages, validity
	}
	original, validity := snapshot("inbox")
	if len(original) != 1 || validity == 0 {
		t.Fatalf("initial mailbox = %v, validity = %d", original, validity)
	}
	selected := &imapMailbox{imapUser: imapUser{app: a, owner: owner}, folder: "inbox", messages: original}
	set := new(imap.SeqSet)
	set.AddNum(original[0].Uid)
	if err := selected.UpdateMessagesFlags(true, set, imap.AddFlags, []string{imap.FlaggedFlag}); err != nil {
		t.Fatal(err)
	}
	move := func(folder string) {
		t.Helper()
		r := httptest.NewRequest("PATCH", "/", strings.NewReader(`{"folder":"`+folder+`"}`))
		r.Header.Set("Content-Type", "application/json")
		r.SetPathValue("id", strconv.FormatInt(original[0].id, 10))
		w := httptest.NewRecorder()
		a.update(w, r, owner)
		if w.Code != 204 {
			t.Fatalf("mailbox move failed: %d %s", w.Code, w.Body)
		}
	}
	move("archive")
	if err := selected.UpdateMessagesFlags(true, set, imap.AddFlags, []string{imap.SeenFlag}); !errors.Is(err, errIMAPMailboxChanged) {
		t.Fatalf("STORE against stale UID = %v, want membership change", err)
	}
	archived, _ := snapshot("archive")
	if len(archived) != 1 || len(archived[0].Flags) != 1 || archived[0].Flags[0] != imap.FlaggedFlag {
		t.Fatalf("stale STORE changed archived flags: %v", archived)
	}
	if got, _ := snapshot("inbox"); len(got) != 0 {
		t.Fatalf("archived message remained in inbox: %v", got)
	}
	move("restore")
	restored, nextValidity := snapshot("inbox")
	again, _ := snapshot("inbox")
	if len(restored) != 1 || len(again) != 1 || restored[0].Uid <= original[0].Uid ||
		restored[0].Uid != again[0].Uid || nextValidity != validity {
		t.Fatalf("original = %v, restored = %v, reopened = %v, validity = %d/%d",
			original, restored, again, validity, nextValidity)
	}
}
