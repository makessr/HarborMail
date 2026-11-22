package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestLetterPrintAndSource(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "mail.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const owner, other, password = "print@harbor.test", "other@harbor.test", "test-print-password"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, dbDriver: "sqlite", domain: "harbor.test", accounts: map[string][]byte{owner: hash, other: hash}}
	// Some stored addresses have an encoded display name inside a quoted phrase.
	const encodedSender = `"=?utf-8?B?5pyI5oOF?=" <` + other + ">"
	const raw = "From: " + encodedSender + "\r\nCc: team@harbor.test\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
		"<p>Rich body</p><script>malicious()</script><img src='https://track.example.invalid/pixel' onload='bad()'>"
	row, err := db.Exec(`INSERT INTO letters(owner,sender,recipient,subject,body,stamp,folder,origin,raw)
		VALUES(?,?,?,?,?,?,?,?,?)`, owner, encodedSender, owner, "<mail> & subject", "Plain body <tag>", 1760000000, "inbox", "inbox", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	id, err := row.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	web := httptest.NewServer(a.routes())
	defer web.Close()
	login := func(email string) *http.Client {
		t.Helper()
		jar, _ := cookiejar.New(nil)
		client := &http.Client{Jar: jar}
		postMailOperation(t, client, web.URL+"/api/login", map[string]string{"email": email, "password": password}, http.StatusOK)
		return client
	}
	root := web.URL + "/api/letters/" + strconv.FormatInt(id, 10)
	client := login(owner)
	list, err := client.Get(web.URL + "/api/letters?folder=inbox")
	if err != nil {
		t.Fatal(err)
	}
	defer list.Body.Close()
	var inbox struct{ Letters []letter }
	if err := json.NewDecoder(list.Body).Decode(&inbox); err != nil || list.StatusCode != http.StatusOK || len(inbox.Letters) != 1 {
		t.Fatalf("unable to read the stored sender from the inbox: %d (%v)", list.StatusCode, err)
	}
	if item := inbox.Letters[0]; item.From != encodedSender || item.SenderName != "月情" || item.SenderAddress != other {
		t.Fatalf("unexpected stored sender: %q / %q / %q", item.From, item.SenderName, item.SenderAddress)
	}
	get := func(client *http.Client, path string, code int) (http.Header, string) {
		t.Helper()
		res, err := client.Get(root + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != code {
			t.Fatalf("GET %s = %d (%v): %s", path, res.StatusCode, err, body)
		}
		return res.Header, string(body)
	}

	otherClient := login(other)
	_, detail := get(client, "", http.StatusOK)
	var opened letter
	if err := json.Unmarshal([]byte(detail), &opened); err != nil || opened.SenderName != "月情" || opened.SenderAddress != other {
		t.Fatalf("unexpected sender in opened letter: %q / %q (%v)", opened.SenderName, opened.SenderAddress, err)
	}
	get(otherClient, "/print?format=html", http.StatusNotFound)
	get(otherClient, "/raw?view=1", http.StatusNotFound)
	get(web.Client(), "/print", http.StatusUnauthorized)
	headers, html := get(client, "/print?format=html&print=1", http.StatusOK)
	if headers.Get("Cache-Control") != "no-store" || !strings.Contains(headers.Get("Content-Security-Policy"), "img-src data:") {
		t.Fatalf("unexpected print headers: %v", headers)
	}
	for _, want := range []string{`data-auto-print="true"`, "Rich body", "team@harbor.test", "&lt;mail&gt; &amp; subject", "月情 &lt;other@harbor.test&gt;"} {
		if !strings.Contains(html, want) {
			t.Errorf("print page lacks %q: %s", want, html)
		}
	}
	for _, forbidden := range []string{"malicious()", "track.example.invalid", "onload=", "Plain body"} {
		if strings.Contains(html, forbidden) {
			t.Errorf("unexpected %q in print page", forbidden)
		}
	}
	_, plain := get(client, "/print?format=text", http.StatusOK)
	if !strings.Contains(plain, "Plain body &lt;tag&gt;") || strings.Contains(plain, "Rich body") {
		t.Fatalf("plain print output: %s", plain)
	}
	headers, original := get(client, "/raw?view=1", http.StatusOK)
	if original != raw || headers.Get("Content-Type") != "text/plain; charset=utf-8" || headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("original source was not served as literal text: %v", headers)
	}
	headers, downloaded := get(client, "/raw", http.StatusOK)
	if downloaded != raw || !strings.HasPrefix(headers.Get("Content-Disposition"), "attachment;") {
		t.Fatalf("raw download changed: %v", headers)
	}
}
