package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestQueuedDraftUsesRenamedMailboxAddress(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testRenamedMailboxDraft(t, "sqlite", filepath.Join(t.TempDir(), "mail.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testRenamedMailboxDraft(t, "mysql", dsn) })
	}
}

func testRenamedMailboxDraft(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, dbDriver: driver, domain: "harbor.test", relay: domainRelay{Address: "relay.harbor.test:587"}}
	name := token()[:16]
	oldOwner, newOwner := name+"@harbor.test", name+"@renamed.harbor.test"
	hash := []byte("synthetic credential")
	if _, err := db.Exec(`INSERT INTO managed_accounts(email,password_hash) VALUES(?,?)`, oldOwner, hash); err != nil {
		t.Fatal(err)
	}
	const parent = "original@harbor.test"
	attachment := []byte{0, 5, 255, 10, 13}
	m := letter{From: oldOwner, To: "recipient@example.test", Subject: "Renamed draft", Body: "Synthetic body",
		Date: time.Now().Unix(), reply: replyHeaders{parents: []string{parent}}}
	raw, err := encodeLetter(m, a.domain, []uploadedAttachment{{name: "fixture.bin", data: attachment}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	id, err := insertWebDraft(ctx, tx, oldOwner, m, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.renameManagedMailbox(ctx, tx, oldOwner, newOwner, hash, false); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/drafts/id/send", nil)
	req.Header.Set("If-Match", "1")
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	response := httptest.NewRecorder()
	a.sendDraft(response, req, newOwner)
	if response.Code != http.StatusAccepted {
		t.Fatalf("send migrated draft: HTTP %d %s", response.Code, response.Body)
	}
	var queued, stored []byte
	if err := db.QueryRow(`SELECT raw FROM outbox WHERE sender=?`, newOwner).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT raw FROM letters WHERE id=? AND owner=?`, id, newOwner).Scan(&stored); err != nil || !bytes.Equal(raw, stored) {
		t.Fatalf("the original draft was changed: %v", err)
	}
	message, err := mail.ReadMessage(bytes.NewReader(queued))
	if err != nil {
		t.Fatal(err)
	}
	from, err := message.Header.AddressList("From")
	if err != nil || len(from) != 1 || from[0].Address != newOwner || len(message.Header["From"]) != 1 {
		t.Fatalf("queued From = %v, want %s (%v)", message.Header["From"], newOwner, err)
	}
	if message.Header.Get("In-Reply-To") != "<"+parent+">" {
		t.Fatal("queued message lost its reply reference")
	}
	var got []byte
	visitAttachments(queued, func(_ attachmentInfo, part io.Reader) bool {
		got, err = io.ReadAll(part)
		return false
	})
	if err != nil || !bytes.Equal(got, attachment) {
		t.Fatalf("queued attachment changed: %v (%v)", got, err)
	}
}

func TestDraftCreationReplay(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { testDraftCreationReplay(t, "sqlite", filepath.Join(t.TempDir(), "mail.db")) })
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testDraftCreationReplay(t, "mysql", dsn) })
	}
}

func testDraftCreationReplay(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, domain: "harbor.test"}
	owner := token()[:16] + "@draft.harbor.test"
	key := token()[:32]
	create := func(actor, requestKey, subject string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/drafts", strings.NewReader(`{"subject":"`+subject+`"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", requestKey)
		w := httptest.NewRecorder()
		a.createDraft(w, r, actor)
		return w
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- create(owner, key, "original text") }()
	second := create(owner, key, "original text")
	original := <-first
	if original.Code == 200 {
		original, second = second, original
	}
	if original.Code != 201 || second.Code != 200 || original.Body.String() != second.Body.String() {
		t.Fatalf("concurrent receipt mismatch: %d %s, %d %s", original.Code, original.Body, second.Code, second.Body)
	}
	var receipt map[string]int64
	if err := json.Unmarshal(original.Body.Bytes(), &receipt); err != nil || receipt["id"] <= 0 || receipt["draftVersion"] != 1 {
		t.Fatalf("creation receipt: %v (%v)", receipt, err)
	}
	id := receipt["id"]
	if response := create("other-"+owner, key, "other account"); response.Code != 201 || response.Body.String() == original.Body.String() {
		t.Fatalf("receipt leaked across accounts: %d %s", response.Code, response.Body)
	}
	if response := create(owner, "short", "invalid"); response.Code != 400 {
		t.Fatalf("invalid key: %d %s", response.Code, response.Body)
	}
	repeat := create(owner, key, "replay must not overwrite")
	if repeat.Code != 200 || repeat.Body.String() != original.Body.String() {
		t.Fatalf("retry did not return creation receipt: %d %s", repeat.Code, repeat.Body)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM letters WHERE owner=? AND subject='original text' AND folder='drafts'`, owner).
		Scan(&count); err != nil || count != 1 {
		t.Fatalf("created drafts = %d, err = %v", count, err)
	}
	trash := "trash"
	if err := a.changeLetters(t.Context(), owner, []int64{id}, letterChange{Folder: &trash}); err != nil {
		t.Fatal(err)
	}
	if response := create(owner, key, "must not resurrect"); response.Code != 409 {
		t.Fatalf("replay after trash: %d %s", response.Code, response.Body)
	}
}

func TestSendingDraftQueuesExactlyOnce(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db, domain: "harbor.test"}
	const owner = "sender@harbor.test"
	create := httptest.NewRequest(http.MethodPost, "/api/drafts", strings.NewReader(
		`{"to":"recipient@example.test","subject":"Queue draft","body":"Test body"}`))
	create.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	a.createDraft(created, create, owner)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: got %d %s", created.Code, created.Body)
	}
	var record map[string]int64
	if err := json.Unmarshal(created.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/drafts/send", nil)
		req.Header.Set("If-Match", "1")
		req.SetPathValue("id", strconv.FormatInt(record["id"], 10))
		response := httptest.NewRecorder()
		a.sendDraft(response, req, owner)
		return response
	}
	if response := send(); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("send without relay: got %d %s", response.Code, response.Body)
	}
	a.relay.Address = "relay.example.test:587"
	if response := send(); response.Code != http.StatusAccepted {
		t.Fatalf("retry after setting relay: got %d %s", response.Code, response.Body)
	}
	if response := send(); response.Code != http.StatusNotFound {
		t.Fatalf("repeated send: got %d %s", response.Code, response.Body)
	}
	var folder string
	if err := db.QueryRow(`SELECT folder FROM letters WHERE id=?`, record["id"]).Scan(&folder); err != nil || folder != "trash" {
		t.Fatalf("draft folder = %s, err = %v", folder, err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM outbox WHERE sender=? AND subject='Queue draft'`, owner).Scan(&count); err != nil || count != 1 {
		t.Fatalf("queued copies = %d, err = %v", count, err)
	}
}
