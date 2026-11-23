package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"

	"harbormail/internal/mailmime"
)

func TestAliasAndRichDraftLifecycle(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { testAliasAndRichDraft(t, "sqlite", filepath.Join(t.TempDir(), "mail.db")) })
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testAliasAndRichDraft(t, "mysql", dsn) })
	}
}

func testAliasAndRichDraft(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	prefix := token()[:10]
	owner, other := prefix+"@harbor.test", prefix+"@person.test"
	alias := prefix + "@alias.test"
	a := &app{db: db, dbDriver: driver, domain: "harbor.test", accounts: map[string][]byte{owner: {}, other: {}},
		aliases: map[string]string{alias: owner}, sendAliases: map[string]bool{alias: true},
		domainRelays: map[string]domainRelay{"alias.test": {Address: "relay.invalid:587"}}}
	const displayName = "王, Jason"
	if _, err := db.Exec(`INSERT INTO mailbox_preferences(owner,revision,theme,density,read_mode,sender_display_name)
		VALUES(?,1,'light','comfortable','auto',?)`, owner, displayName); err != nil {
		t.Fatal(err)
	}
	assertFrom := func(raw []byte) {
		t.Helper()
		header := letterHeader(raw)
		from, err := header.AddressList("From")
		if err != nil || len(from) != 1 || from[0].Name != displayName || from[0].Address != alias {
			t.Fatalf("From header = %q, want %q <%s>: %v", header.Get("From"), displayName, alias, err)
		}
	}
	const body = "**bold**\n- One\n> Quote\n[link](https://example.com)\ntest body"
	plan := "?sendAt=" + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	post := func(actor, to, params, subject string, want int) int64 {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"author": alias, "to": to, "subject": subject,
			"body": body, "formatted": true})
		r := httptest.NewRequest("POST", "/api/letters"+params, strings.NewReader(string(payload)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		a.send(w, r, actor)
		if w.Code != want {
			t.Fatalf("send got %d, want %d: %s", w.Code, want, w.Body)
		}
		var result struct{ Queued int64 }
		if want == http.StatusAccepted {
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Queued == 0 {
				t.Fatalf("missing queued ID: %s", w.Body)
			}
		}
		return result.Queued
	}
	post(other, owner, "", "Forged sender", http.StatusUnprocessableEntity)
	queued := post(owner, other, plan, "Restored rich draft", http.StatusAccepted)
	draft, err := a.restoreScheduled(t.Context(), owner, queued)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := a.rawLetter(owner, draft)
	if err != nil {
		t.Fatal(err)
	}
	header := letterHeader(raw)
	if replyAddress(raw, alias) != alias || header.Get("X-Harbor-Format") != "rich" || header.Get("X-Harbor-Send-As") != alias {
		t.Fatal("restored draft lost the sender identity or rich format")
	}
	r := httptest.NewRequest("POST", "/api/drafts/send", nil)
	r.Header.Set("If-Match", "1")
	r.SetPathValue("id", strconv.FormatInt(draft, 10))
	w := httptest.NewRecorder()
	a.sendDraft(w, r, owner)
	if w.Code != http.StatusCreated {
		t.Fatalf("send restored draft: %d, %s", w.Code, w.Body)
	}
	var from, plain string
	if err := db.QueryRow(`SELECT sender,body,raw FROM letters WHERE owner=? AND folder='inbox' AND subject=?`, other, "Restored rich draft").
		Scan(&from, &plain, &raw); err != nil {
		t.Fatal(err)
	}
	if from != alias || strings.Contains(plain, "**") || !strings.Contains(plain, "https://example.com") ||
		!mailmime.InspectHTML(raw, false).HasHTML {
		t.Fatalf("delivered message has incorrect sender or parts: %q, %q", from, plain)
	}
	assertFrom(raw)

	outbox := post(owner, other, plan, "Queued rich mail", http.StatusAccepted)
	if _, err := db.Exec(`UPDATE outbox SET due=0 WHERE id=?`, outbox); err != nil {
		t.Fatal(err)
	}
	job, err := a.claimOutbox()
	if err != nil || job == nil || job.message.ID != outbox || job.message.wireFrom() != alias {
		t.Fatalf("missing scheduled alias shipment: %v (%v)", job, err)
	}
	assertFrom(job.raw)
	if err := a.processOutbox(job, nil); err != nil {
		t.Fatal(err)
	}
	var link int64
	if err := db.QueryRow(`SELECT delivery_job_id FROM letters WHERE owner=? AND folder='sent' AND subject=?`, owner, "Queued rich mail").Scan(&link); err != nil || link != outbox {
		t.Fatalf("sent timeline link %d: %v", link, err)
	}
	events, err := readDeliveryTimeline(t.Context(), db, owner, link)
	if err != nil || len(events) != 2 || events[0].State != "local_delivered" || events[1].State != "enqueued" {
		t.Fatalf("unexpected timeline: %+v, %v", events, err)
	}
	if otherEvents, err := readDeliveryTimeline(t.Context(), db, other, link); err != nil || len(otherEvents) != 0 {
		t.Fatalf("foreign timeline result: %+v, %v", otherEvents, err)
	}

	a.dkimCipher = testDKIMCipher(t)
	selector, public := installTestDKIMKey(t, a, "alias.test")
	external := post(owner, prefix+"@outside.test", "", "Authorized sender", http.StatusAccepted)
	job, err = a.claimOutbox()
	if err != nil || job == nil || job.message.ID != external || a.sendRelay(job.message.wireFrom()).Address == "" {
		t.Fatalf("alias relay not selected: %v", err)
	}
	wireFrom, captured := make(chan string, 1), make(chan []byte, 1)
	if err := a.processOutbox(job, serveTestRelay(t, a, false, relayFixture{
		replyCode: 250, onMail: func(from string) { wireFrom <- from }, onData: func(raw []byte) { captured <- raw },
	})); err != nil {
		t.Fatal(err)
	}
	select {
	case from := <-wireFrom:
		if from != alias {
			t.Fatalf("SMTP MAIL FROM = %q", from)
		}
	default:
		t.Fatal("SMTP envelope not received")
	}
	select {
	case received := <-captured:
		assertFrom(received)
		verifyTestDKIM(t, received, "alias.test", selector, public)
	default:
		t.Fatal("SMTP message not received")
	}
	events, err = readDeliveryTimeline(t.Context(), db, owner, external)
	if err != nil || len(events) != 3 || events[0].State != "sent" || events[0].Code != 250 || events[0].Route != "relay" ||
		events[0].Address != prefix+"@outside.test" {
		t.Fatalf("missing relay acceptance timeline: %+v, %v", events, err)
	}

	external = post(owner, prefix+"@outside.test", "", "Revoked sender", http.StatusAccepted)
	job, err = a.claimOutbox()
	if err != nil || job == nil || job.message.ID != external {
		t.Fatalf("alias-specific relay did not make outbox job eligible: %v", err)
	}
	delete(a.sendAliases, alias)
	err = a.processOutbox(job, func(context.Context) (*smtp.Client, func(), error) {
		t.Fatal("relay attempted an unauthorized sender")
		return nil, nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var status string
	if err := db.QueryRow(`SELECT state FROM outbox WHERE id=?`, external).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("revoked identity job state = %q, %v", status, err)
	}
}

func TestRichComposeEscapingAndMultipartAttachment(t *testing.T) {
	source := "**<script>**\n[unsafe](javascript:alert)\n[site](https://example.com)\n- One\n> Quote"
	plain, rich := renderRichCompose(source)
	if strings.Contains(rich, "<script>") || strings.Contains(rich, `href="javascript:`) ||
		!strings.Contains(rich, "<ul><li>One</li></ul>") || !strings.Contains(plain, "site (https://example.com)") {
		t.Fatalf("unexpected rich/plain output: %q, %q", rich, plain)
	}
	raw, err := encodeLetter(letter{From: "sender@harbor.test", Body: source, formatted: true}, "harbor.test",
		[]uploadedAttachment{{name: "test.txt", data: []byte("fixture")}})
	if err != nil {
		t.Fatal(err)
	}
	detail := inspectMailDetail(raw)
	if !detail.HasHTML || !detail.HasText || len(detail.Attachments) != 1 || detail.Attachments[0].Name != "test.txt" {
		t.Fatalf("rich mail lost body or attachment: %+v", detail)
	}
}
