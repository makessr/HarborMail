package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMailLabels(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { testMailLabels(t, "sqlite", filepath.Join(t.TempDir(), "mail.db")) })
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testMailLabels(t, "mysql", dsn) })
	}
}

func testMailLabels(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	suffix := token()[:12] + "@labels.harbor.test"
	owner, other := "a-"+suffix, "b-"+suffix
	a := &app{db: db, accounts: map[string][]byte{owner: nil}}
	seed := func(account, folder string) int64 {
		t.Helper()
		result, err := db.Exec(`INSERT INTO letters(owner,sender,recipient,subject,body,stamp,folder,origin,raw)
			VALUES (?,'sender@example.test',?,'Project meeting','payload',1,?,'inbox','')`, account, account, folder)
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	ownedID, otherID, draftID := seed(owner, "inbox"), seed(other, "inbox"), seed(owner, "drafts")
	request := func(handler func(http.ResponseWriter, *http.Request, string), method, path, actor, body string, letterID, labelID int64, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.SetPathValue("id", fmt.Sprint(letterID))
		r.SetPathValue("label", fmt.Sprint(labelID))
		w := httptest.NewRecorder()
		handler(w, r, actor)
		if w.Code != want {
			t.Fatalf("%s %s returned %d, want %d: %s", method, path, w.Code, want, w.Body)
		}
		return w
	}
	newLabel := func(actor, body string) mailLabel {
		t.Helper()
		w := request(a.saveMailLabel, "POST", "/api/labels", actor, body, 0, 0, 201)
		var label mailLabel
		if err := json.Unmarshal(w.Body.Bytes(), &label); err != nil || label.ID < 1 {
			t.Fatalf("create label: %s (%v)", w.Body, err)
		}
		return label
	}
	project := newLabel(owner, `{"name":"  Project  "}`)
	urgent := newLabel(owner, `{"name":"待处理"}`)
	foreign := newLabel(other, `{"name":"Project"}`)
	request(a.saveMailLabel, "POST", "/api/labels", owner, `{"name":"project"}`, 0, 0, 409)
	request(a.saveMailLabel, "PUT", "/api/labels/id", other, `{"name":"changed"}`, 0, project.ID, 404)
	change := func(method string, letter, label int64, want int) {
		t.Helper()
		request(a.changeMailLabel, method, "/api/letters/id/labels/id", owner, "", letter, label, want)
	}
	change("PUT", otherID, project.ID, 404)
	change("PUT", ownedID, foreign.ID, 404)
	change("PUT", draftID, project.ID, 404)
	change("PUT", ownedID, project.ID, 204)
	change("PUT", ownedID, project.ID, 204)
	change("PUT", ownedID, urgent.ID, 204)
	checkList := func(actor, search string, label int64, count int) {
		t.Helper()
		path := fmt.Sprintf("/api/letters?folder=all&label=%d&q=%s", label, search)
		w := request(a.list, "GET", path, actor, "", 0, 0, 200)
		var result struct{ Letters []letter }
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Letters) != count {
			t.Fatalf("filter %s: %s (%v)", path, w.Body, err)
		}
		if count == 1 && (result.Letters[0].ID != ownedID || result.Letters[0].Folder != "inbox") {
			t.Fatalf("label changed the selected letter or its folder: %#v", result.Letters[0])
		}
	}
	checkList(owner, "meeting", project.ID, 1)
	checkList(owner, "unrelated", project.ID, 0)
	checkList(other, "meeting", project.ID, 0)
	request(a.saveMailLabel, "PUT", "/api/labels/id", owner, `{"name":"项目文档"}`, 0, project.ID, 200)
	w := request(a.read, "GET", "/api/letters/id", owner, "", ownedID, 0, 200)
	var item letter
	if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil || len(item.Labels) != 2 || !strings.Contains(w.Body.String(), "项目文档") {
		t.Fatalf("read labels: %s (%v)", w.Body, err)
	}
	for _, move := range []struct {
		folder string
		count  int
	}{{"trash", 0}, {"restore", 1}} {
		if err := a.changeLetters(t.Context(), owner, []int64{ownedID}, letterChange{Folder: &move.folder}); err != nil {
			t.Fatal(err)
		}
		checkList(owner, "meeting", project.ID, move.count)
	}
	change("DELETE", ownedID, project.ID, 204)
	change("DELETE", ownedID, project.ID, 204)
	checkList(owner, "meeting", project.ID, 0)
	checkList(owner, "meeting", urgent.ID, 1)
	request(a.deleteMailLabel, "DELETE", "/api/labels/id", owner, "", 0, urgent.ID, 204)
	checkList(owner, "meeting", urgent.ID, 0)
	var links int
	if err := db.QueryRow(`SELECT count(*) FROM mail_label_links WHERE letter_id=?`, ownedID).Scan(&links); err != nil || links != 0 {
		t.Fatalf("label cleanup: %d links, %v", links, err)
	}
	request(a.read, "GET", "/api/letters/id", owner, "", ownedID, 0, 200)

	batch := func(add bool, first, second int64, want int) {
		t.Helper()
		body := fmt.Sprintf(`{"ids":[%d,%d],"label":{"id":%d,"add":%t}}`, first, second, project.ID, add)
		request(a.updateBatch, "PATCH", "/api/letters", owner, body, 0, 0, want)
	}
	checkMembers := func(want int) {
		t.Helper()
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM mail_label_links WHERE label_id=?`, project.ID).Scan(&count); err != nil || count != want {
			t.Fatalf("label membership: got %d, want %d (%v)", count, want, err)
		}
	}
	batch(true, ownedID, otherID, 404)
	checkMembers(0)
	secondID := seed(owner, "sent")
	batch(true, secondID, ownedID, 204)
	batch(true, ownedID, secondID, 204)
	checkMembers(2)
	batch(false, ownedID, otherID, 404)
	checkMembers(2)
	batch(false, ownedID, secondID, 204)
	checkMembers(0)

	ctx := t.Context()
	rule := mailRule{Order: 10, Enabled: true, Field: "subject", Value: "rule label", Action: "label", LabelID: project.ID}
	if _, err := a.saveMailRule(ctx, other, rule); !errors.Is(err, errRuleLabel) {
		t.Fatalf("foreign rule label: %v", err)
	}
	saved, err := a.saveMailRule(ctx, owner, rule)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.saveMailRule(ctx, owner, mailRule{Order: 20, Enabled: true, Field: "subject", Value: rule.Value, Action: "star"}); err != nil {
		t.Fatal(err)
	}
	request(a.deleteMailLabel, "DELETE", "/api/labels/id", owner, "", 0, project.ID, 409)
	message := letter{From: other, To: owner, Subject: "Rule label on arrival", Body: "automatic", Date: 2}
	if err := a.deliverLocal(message, owner, []byte("Subject: Rule label on arrival\r\n\r\nautomatic\r\n")); err != nil {
		t.Fatal(err)
	}
	checkMembers(1)
	var folder string
	var starred bool
	if err := db.QueryRow(`SELECT l.folder,l.star FROM letters l JOIN mail_label_links ml ON ml.letter_id=l.id
		WHERE l.owner=? AND ml.label_id=?`, owner, project.ID).Scan(&folder, &starred); err != nil || folder != "inbox" || starred {
		t.Fatalf("first rule should label without moving or starring: %q %v (%v)", folder, starred, err)
	}
	request(a.deleteRuleHTTP, "DELETE", "/api/rules/id", owner, "", saved.ID, 0, 204)
	request(a.deleteMailLabel, "DELETE", "/api/labels/id", owner, "", 0, project.ID, 204)
	checkMembers(0)
}
