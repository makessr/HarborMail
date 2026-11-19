package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestLetterDetailKeepsBlindAliasesOutOfForwardHeaders(t *testing.T) {
	const sender, visible, copied, blind = "sender@harbor.test", "visible@harbor.test", "copy@harbor.test", "blind@harbor.test"
	const alias = "private-alias@harbor.test"
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, domain: "harbor.test",
		accounts: map[string][]byte{sender: {}, visible: {}, copied: {}, blind: {}},
		aliases:  map[string]string{alias: blind}}

	for _, tc := range []struct {
		subject string
		public  messageHeaderRecipients
	}{
		{"multiple recipients", messageHeaderRecipients{To: visible, Cc: copied}},
		{"blind recipient only", messageHeaderRecipients{}},
	} {
		t.Run(tc.subject, func(t *testing.T) {
			payload, err := json.Marshal(map[string]string{
				"to": tc.public.To, "cc": tc.public.Cc, "bcc": alias, "subject": tc.subject, "body": "Hello",
			})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/api/letters", strings.NewReader(string(payload)))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			a.send(w, r, sender)
			if w.Code < 200 || w.Code >= 300 {
				t.Fatalf("send: HTTP %d, %s", w.Code, w.Body)
			}

			owners := []string{sender, blind}
			if tc.public.To != "" {
				owners = append(owners, visible, copied)
			}
			for _, owner := range owners {
				var id int64
				if err := db.QueryRow("SELECT id FROM letters WHERE owner=? AND subject=?", owner, tc.subject).Scan(&id); err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest(http.MethodGet, "/api/letters/"+strconv.FormatInt(id, 10), nil)
				r.SetPathValue("id", strconv.FormatInt(id, 10))
				w := httptest.NewRecorder()
				a.read(w, r, owner)
				var detail struct {
					letter
					HeaderRecipients *messageHeaderRecipients `json:"headerRecipients"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &detail); w.Code != http.StatusOK || err != nil {
					t.Fatalf("read for %s: HTTP %d, %v", owner, w.Code, err)
				}
				if detail.HeaderRecipients == nil || *detail.HeaderRecipients != tc.public {
					t.Fatalf("read for %s: incorrect public headers: %+v", owner, detail.HeaderRecipients)
				}
				wantBcc := ""
				if owner == sender {
					wantBcc = alias
				}
				if detail.Bcc != wantBcc || (owner == blind && detail.To != alias) {
					t.Fatalf("read for %s: unexpected stored To %q or Bcc %q", owner, detail.To, detail.Bcc)
				}
				raw, err := a.rawLetter(owner, id)
				if err != nil || strings.Contains(string(raw), alias) || strings.Contains(string(raw), "\nBcc:") {
					t.Fatalf("unexpected original message for %s: %v", owner, err)
				}
			}
		})
	}
}

func TestBatchMailChangesRollbackOnForeignLetter(t *testing.T) {
	const owner = "owner@harbor.test"
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db}
	if _, err := db.Exec(`INSERT INTO letters (id,owner,sender,recipient,subject,body,stamp,folder,origin,raw) VALUES
		(1,?,'sender@harbor.test',?,'test','',1,'inbox','inbox',''),
		(2,'other@harbor.test','sender@harbor.test','other@harbor.test','test','',1,'inbox','inbox','')`, owner, owner); err != nil {
		t.Fatal(err)
	}
	change := func(body string) int {
		r := httptest.NewRequest(http.MethodPatch, "/api/letters", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		a.updateBatch(w, r, owner)
		return w.Code
	}
	if code := change(`{"ids":[1,2],"folder":"archive","star":true}`); code != http.StatusNotFound {
		t.Fatalf("batch with foreign letter returned %d", code)
	}
	var folder string
	var star bool
	if err := db.QueryRow(`SELECT folder,star FROM letters WHERE id=1`).Scan(&folder, &star); err != nil || folder != "inbox" || star {
		t.Fatalf("failed batch changed owned letter: %q %v %v", folder, star, err)
	}
	if code := change(`{"ids":[1,1],"read":false}`); code != http.StatusBadRequest {
		t.Fatalf("duplicate IDs returned %d", code)
	}
	if code := change(`{"ids":[1],"folder":"archive","star":true}`); code != http.StatusNoContent {
		t.Fatalf("valid batch returned %d", code)
	}
	if err := db.QueryRow(`SELECT folder,star FROM letters WHERE id=1`).Scan(&folder, &star); err != nil || folder != "archive" || !star {
		t.Fatalf("owned letter not updated: %q %v %v", folder, star, err)
	}
}

func TestAllMailFiltersCombineAndKeepPrivateFoldersOut(t *testing.T) {
	const owner = "owl@harbor.test"
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, entry := range []struct {
		folder string
		stamp  int
		seen   bool
	}{
		{"inbox", 10, false}, {"sent", 19, false}, {"archive", 20, false}, {"archive", 15, true},
		{"trash", 12, false}, {"junk", 12, false}, {"drafts", 12, false},
	} {
		_, err := db.Exec(`INSERT INTO letters (owner,sender,recipient,subject,body,stamp,folder,origin,seen,raw)
			VALUES (?,?,?,'meeting','payload',?,?,'inbox',?,'')`,
			owner, "Sender@example.test", owner, entry.stamp, entry.folder, entry.seen)
		if err != nil {
			t.Fatal(err)
		}
	}
	a := &app{db: db}
	r := httptest.NewRequest(http.MethodGet,
		"/api/letters?folder=all&sender=sender%40example.test&recipient=owl&seen=unread&since=10&until=20&q=meeting", nil)
	w := httptest.NewRecorder()
	a.list(w, r, owner)
	var result struct{ Letters []letter }
	if err := json.Unmarshal(w.Body.Bytes(), &result); w.Code != http.StatusOK || err != nil {
		t.Fatalf("list: HTTP %d, %v", w.Code, err)
	}
	var got []string
	for _, m := range result.Letters {
		got = append(got, m.Folder)
	}
	if !slices.Equal(got, []string{"sent", "inbox"}) {
		t.Fatalf("matching folders: %v", got)
	}
}
