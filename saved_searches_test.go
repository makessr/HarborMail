package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSavedSearches(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { testSavedSearches(t, "sqlite", filepath.Join(t.TempDir(), "mail.db")) })
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testSavedSearches(t, "mysql", dsn) })
	}
}

func testSavedSearches(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := &app{db: db}
	owner := token()[:16] + "@search.harbor.test"
	other := "other-" + owner
	folder, err := a.saveCustomFolder(t.Context(), owner, "", "资料")
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, actor string, item savedSearch, want int) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(method, "/api/saved-searches", strings.NewReader(string(body)))
		r.SetPathValue("id", fmt.Sprint(item.ID))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("If-Match", fmt.Sprint(item.Revision))
		w := httptest.NewRecorder()
		switch method {
		case http.MethodGet:
			a.listSavedSearches(w, r, actor)
		case http.MethodDelete:
			a.deleteSavedSearch(w, r, actor)
		default:
			a.saveSearch(w, r, actor)
		}
		if w.Code != want {
			t.Fatalf("%s search: got HTTP %d, want %d (%s)", method, w.Code, want, w.Body)
		}
		return w
	}
	list := func(actor string) []savedSearch {
		t.Helper()
		w := call("GET", actor, savedSearch{}, 200)
		var items []savedSearch
		if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
			t.Fatal(err)
		}
		return items
	}
	settings := savedSearch{Name: "  Project  ", Folder: folder.ID, Query: " 周报 ",
		Filters: savedSearchFilters{Sender: " example.test ", Seen: "unread", Start: "2026-11-18", End: "2026-11-18"}}
	call("POST", other, settings, 404)
	w := call("POST", owner, settings, 201)
	var saved savedSearch
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || saved.ID < 1 || saved.Revision != 1 ||
		saved.Name != "Project" || saved.Query != "周报" || saved.Filters.Sender != "example.test" {
		t.Fatalf("saved search: %s (%v)", w.Body, err)
	}
	settings.Name = "project"
	call("POST", owner, settings, 409)
	settings.Name = "Outbox"
	settings.Folder = "outbox"
	call("POST", owner, settings, 400)
	if items := list(other); items == nil || len(items) != 0 {
		t.Fatalf("other account sees saved searches: %v", items)
	}
	call("PUT", other, saved, 404)
	call("DELETE", other, saved, 404)
	if err := a.deleteCustomFolder(t.Context(), owner, folder.ID); err != nil {
		t.Fatal(err)
	}
	if items := list(owner); len(items) != 1 || !items[0].Unavailable || items[0].Query != "周报" {
		t.Fatalf("deleted scope should preserve recoverable criteria: %v", items)
	}
	saved.Folder, saved.Name = "inbox", "未读周报"
	call("PUT", owner, saved, 200)
	call("PUT", owner, saved, 409)
	call("DELETE", owner, saved, 409)
	items := list(owner)
	if len(items) != 1 || items[0].Revision != 2 || items[0].Unavailable || items[0].Filters != saved.Filters {
		t.Fatalf("recovered saved search: %v", items)
	}
	call("DELETE", owner, items[0], 204)
	if items := list(owner); len(items) != 0 {
		t.Fatalf("deletion did not clear saved search: %v", items)
	}
}
