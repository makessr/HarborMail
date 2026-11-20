package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestBrowserMailTranslation(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testBrowserMailTranslation(t, "sqlite", filepath.Join(t.TempDir(), "mail.sqlite"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testBrowserMailTranslation(t, "mysql", dsn) })
	}
}

func testBrowserMailTranslation(t *testing.T, driver, dsn string) {
	t.Helper()
	db, err := openDB(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const owner, other, password = "browser-translation@harbor.test", "browser-other@harbor.test", "test-translation-password"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, dbDriver: driver, domain: "harbor.test", accounts: map[string][]byte{owner: hash, other: hash}}
	raw := []byte("From: author@example.test\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Welcome</p><script>hidden</script>")
	var id int64
	result, err := db.Exec(`INSERT INTO letters(owner,sender,recipient,subject,body,stamp,folder,origin,raw)
		VALUES(?,?,?,?,?,?,?,?,?)`, owner, "author@example.test", owner, "Hello", "", 1, "inbox", "inbox", raw)
	if err != nil {
		t.Fatal(err)
	}
	id, err = result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DELETE FROM letters WHERE id=?", id)

	server := httptest.NewServer(a.routes())
	defer server.Close()
	login := func(email string) *http.Client {
		t.Helper()
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar}
		postMailOperation(t, client, server.URL+"/api/login", map[string]string{"email": email, "password": password}, http.StatusOK)
		return client
	}
	endpoint := fmt.Sprintf("%s/api/letters/%d/translate-local", server.URL, id)
	url := endpoint + "?source=en&target=zh-Hans"
	client, otherClient := login(owner), login(other)
	get := func(client *http.Client, url string, status int) browserTranslationSource {
		t.Helper()
		res, err := client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != status {
			t.Fatalf("GET status = %d, want %d", res.StatusCode, status)
		}
		var source browserTranslationSource
		if status == http.StatusOK {
			if err := json.NewDecoder(res.Body).Decode(&source); err != nil || res.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("invalid source response: %+v, %v", source, err)
			}
		}
		return source
	}
	post := func(client *http.Client, input browserTranslationInput, status int) {
		t.Helper()
		payload, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.Post(url, "application/json", strings.NewReader(string(payload)))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != status {
			t.Fatalf("POST status = %d, want %d", res.StatusCode, status)
		}
	}

	get(otherClient, url, http.StatusNotFound)
	get(client, endpoint+"?source=en&target=en", http.StatusBadRequest)
	source := get(client, url, http.StatusOK)
	if source.SourceVersion == "" || source.Cached != nil || source.Original == nil ||
		*source.Original != (translatedEmail{Subject: "Hello", Body: "Welcome"}) {
		t.Fatalf("unexpected original mail: %+v", source)
	}
	translated := translatedEmail{Subject: "你好", Body: "欢迎"}
	input := browserTranslationInput{SourceVersion: source.SourceVersion, Translation: translated}
	post(otherClient, input, http.StatusNotFound)
	post(client, input, http.StatusNoContent)
	cached := get(client, url, http.StatusOK)
	if cached.Cached == nil || *cached.Cached != translated || cached.Original != nil {
		t.Fatalf("unexpected cached translation: %+v", cached)
	}
	anotherLanguage := get(client, endpoint+"?source=fr&target=zh-Hans", http.StatusOK)
	if anotherLanguage.Cached != nil || anotherLanguage.SourceVersion == source.SourceVersion {
		t.Fatal("cache reused a translation from another source language")
	}
	if _, err := db.Exec("UPDATE letters SET subject=? WHERE id=?", "Revised", id); err != nil {
		t.Fatal(err)
	}
	post(client, input, http.StatusConflict)
	if got := get(client, url, http.StatusOK); got.Cached != nil || got.SourceVersion == source.SourceVersion {
		t.Fatal("cache returned a translation of old content")
	}
}
