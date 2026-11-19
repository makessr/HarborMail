package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestContactsArePrivateAndRejectDuplicateAddresses(t *testing.T) {
	const owner = "owner@harbor.test"
	const other = "other@harbor.test"
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db}
	save := func(account, method, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/api/contacts", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		a.saveContact(w, r, account)
		return w
	}
	created := save(owner, http.MethodPost, "", `{"email":"Team@EXAMPLE.test","name":"Żółć 项目","note":"第一行\n第二行"}`)
	var item contact
	if err := json.Unmarshal(created.Body.Bytes(), &item); err != nil || created.Code != http.StatusCreated ||
		item.Email != "Team@example.test" || item.Note != "第一行\n第二行" {
		t.Fatalf("creating contact: HTTP %d, body=%s, err=%v", created.Code, created.Body, err)
	}
	if response := save(owner, http.MethodPost, "", `{"email":"team@example.test"}`); response.Code != http.StatusConflict {
		t.Fatalf("duplicate accepted: HTTP %d", response.Code)
	}
	if response := save(other, http.MethodPost, "", `{"email":"team@example.test","name":"Other contact"}`); response.Code != http.StatusCreated {
		t.Fatalf("contact was incorrectly shared: HTTP %d", response.Code)
	}
	id := strconv.FormatInt(item.ID, 10)
	if response := save(other, http.MethodPut, id, `{"email":"new@example.test"}`); response.Code != http.StatusNotFound {
		t.Fatalf("another account can edit the contact: HTTP %d", response.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/contacts?q="+url.QueryEscape("ŻÓŁĆ"), nil)
	w := httptest.NewRecorder()
	a.listContacts(w, r, owner)
	var result struct{ Contacts []contact }
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Contacts) != 1 || result.Contacts[0] != item {
		t.Fatalf("private search result: %s, %v", w.Body, err)
	}
	r = httptest.NewRequest(http.MethodDelete, "/api/contacts/"+id, nil)
	r.SetPathValue("id", id)
	w = httptest.NewRecorder()
	a.deleteContact(w, r, other)
	if w.Code != http.StatusNotFound {
		t.Fatalf("another account can delete the contact: HTTP %d", w.Code)
	}
}
