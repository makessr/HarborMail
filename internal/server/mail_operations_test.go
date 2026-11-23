package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	imapserver "github.com/emersion/go-imap/server"
	"github.com/emersion/go-smtp"
	"golang.org/x/crypto/bcrypt"
)

func TestInboundPolicyAndQuarantine(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const admin, user, password = "admin@harbor.test", "user@harbor.test", "synthetic-test-password"
	hash, _ := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	a := &app{db: db, accounts: map[string][]byte{admin: hash, user: hash}, admins: map[string]bool{admin: true}}
	answer := make(chan string, 1)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, <-answer)
	}))
	defer worker.Close()
	a.spamScanner, err = newRspamdScanner(worker.URL + "/checkv2")
	if err != nil {
		t.Fatal(err)
	}
	a.scanPolicy = inboundScanPolicy{rejectSpam: true, deferUnavailable: true, virus: "quarantine"}
	s := &delivery{app: a, sender: "sender@example.test", recipients: []mailDestination{{address: user, owner: user}}}
	const raw = "From: sender@example.test\r\nSubject: Test\r\n\r\nSynthetic\r\n"
	for _, test := range []struct {
		verdict string
		code    int
	}{
		{`{"action":"reject"}`, 554},
		{`{"action":"no action","symbols":{"CLAM_VIRUS_FAIL":{}}}`, 451},
		{`{"action":"reject","symbols":{"CLAM_VIRUS":{}}}`, 0},
	} {
		answer <- test.verdict
		err := s.Data(strings.NewReader(raw))
		var smtpErr *smtp.SMTPError
		if (test.code == 0 && err != nil) || (test.code != 0 && (!errors.As(err, &smtpErr) || smtpErr.Code != test.code)) {
			t.Fatalf("verdict %s: %v", test.verdict, err)
		}
	}
	var count, id int
	if err := db.QueryRow("SELECT COUNT(*) FROM letters").Scan(&count); err != nil || count != 0 {
		t.Fatalf("isolated mail leaked to mailbox: %d %v", count, err)
	}
	if err := db.QueryRow("SELECT id FROM inbound_quarantine").Scan(&id); err != nil {
		t.Fatal(err)
	}
	got := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/admin/quarantine", nil)
	a.adminQuarantine(got, req, admin)
	if got.Code != http.StatusOK || bytes.Contains(got.Body.Bytes(), []byte("Synthetic")) {
		t.Fatalf("quarantine list exposed contents: %d", got.Code)
	}
	web := httptest.NewServer(a.routes())
	defer web.Close()
	as := func(owner string) *http.Client {
		t.Helper()
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar}
		postMailOperation(t, c, web.URL+"/api/login", map[string]string{"email": owner, "password": password}, 200)
		return c
	}
	path := web.URL + "/api/admin/quarantine/" + strconv.Itoa(id)
	postMailOperation(t, as(user), path, map[string]string{"action": "release", "password": password}, 403)
	postMailOperation(t, as(admin), path, map[string]string{"action": "release", "password": password}, 204)
	var folder string
	if err := db.QueryRow("SELECT folder FROM letters WHERE owner=?", user).Scan(&folder); err != nil || folder != "junk" {
		t.Fatalf("released mail: %q %v", folder, err)
	}
}

func postMailOperation(t *testing.T, c *http.Client, path string, payload map[string]string, want int) {
	t.Helper()
	data, _ := json.Marshal(payload)
	resp, err := c.Post(path, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("POST %s: %d want %d: %s", path, resp.StatusCode, want, body)
	}
}

func TestSubmissionAliasPermissionRecheckedAtData(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const owner, alias, to = "owner@harbor.test", "team@harbor.test", "other@harbor.test"
	a := &app{db: db, accounts: map[string][]byte{owner: {}, to: {}}, aliases: map[string]string{alias: owner},
		sendAliases: map[string]bool{alias: true}}
	s := submission{app: a, owner: owner}
	raw := "From: " + alias + "\r\nTo: " + to + "\r\nSubject: Alias\r\n\r\nHello\r\n"
	prepare := func() {
		t.Helper()
		if err := s.Mail(alias, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.Rcpt(to, nil); err != nil {
			t.Fatal(err)
		}
	}
	prepare()
	if _, err := db.Exec("INSERT INTO alias_send_permissions(alias,owner,allowed) VALUES(?,?,FALSE)", alias, owner); err != nil {
		t.Fatal(err)
	}
	if err := s.Data(strings.NewReader(raw)); err == nil {
		t.Fatal("SMTP accepted revoked alias")
	}
	if _, err := db.Exec("UPDATE alias_send_permissions SET allowed=TRUE"); err != nil {
		t.Fatal(err)
	}
	prepare()
	if err := s.Data(strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	var sender string
	if err := db.QueryRow("SELECT sender FROM letters WHERE owner=?", to).Scan(&sender); err != nil || sender != alias {
		t.Fatalf("received alias sender: %q %v", sender, err)
	}
}

func TestIMAPImportPreservesFlagsDateAndResumes(t *testing.T) {
	dir := t.TempDir()
	srcDB, err := openDB("sqlite", filepath.Join(dir, "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer srcDB.Close()
	dstDB, err := openDB("sqlite", filepath.Join(dir, "dst.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dstDB.Close()
	const user, pass = "old@harbor.test", "synthetic-test-password"
	hash, _ := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.MinCost)
	source := &app{db: srcDB, accounts: map[string][]byte{user: hash}}
	folder, err := source.saveCustomFolder(t.Context(), user, "", "Projects/历史")
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2021, 4, 14, 8, 30, 0, 0, time.UTC).Unix()
	raw := []byte("From: old@harbor.test\r\nSubject: preserved\r\n\r\nOriginal.\r\n")
	tx, err := srcDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := insert(t.Context(), tx, user, letter{From: user, To: user, Subject: "preserved", Date: stamp, Read: true, Star: true}, folder.ID, raw); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	cert, key := makeReloadTestPEM(t, 1, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}})
	if err != nil {
		t.Fatal(err)
	}
	service := imapserver.New(imapBackend{app: source})
	t.Cleanup(func() { _ = listener.Close(); _ = service.Close() })
	go service.Serve(listener)
	caPath, passPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "password")
	if err := os.WriteFile(caPath, cert, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passPath, []byte(pass), 0600); err != nil {
		t.Fatal(err)
	}
	opt := imapImportOptions{host: listener.Addr().String(), user: user, owner: user, passwordFile: passPath, caFile: caPath, root: "迁移"}
	destination := &app{db: dstDB, accounts: map[string][]byte{user: hash}}
	for range 2 {
		var out bytes.Buffer
		if err := destination.importIMAP(t.Context(), opt, &out); err != nil {
			t.Fatalf("%s %v", out.String(), err)
		}
	}
	var count int
	if err := dstDB.QueryRow("SELECT COUNT(*) FROM letters").Scan(&count); err != nil || count != 1 {
		t.Fatalf("expected one copy: %d %v", count, err)
	}
	var seen, star bool
	var date int64
	var label string
	var got []byte
	err = dstDB.QueryRow("SELECT l.seen,l.star,l.stamp,f.label,l.raw FROM letters l JOIN custom_folders f ON f.folder=l.folder").
		Scan(&seen, &star, &date, &label, &got)
	if err != nil || !seen || !star || date != stamp || label != "迁移/Projects/历史" || !bytes.Equal(raw, got) {
		t.Fatalf("metadata or bytes changed: %v %v %d %q %v", seen, star, date, label, err)
	}
}

func TestLargerWebAttachmentDeliveredAndDownloaded(t *testing.T) {
	db, err := openDB("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const sender, recipient = "sender@harbor.test", "receiver@harbor.test"
	a := &app{db: db, accounts: map[string][]byte{sender: {}, recipient: {}}, sizes: mailSizeLimits{message: 20 << 20, attachments: 12 << 20}}
	data := bytes.Repeat([]byte("H"), 9<<20)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("to", recipient)
	_ = form.WriteField("subject", "Large attachment")
	_ = form.WriteField("body", "Synthetic attachment test.")
	part, err := form.CreateFormFile("attachments", "large.bin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(data)
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/letters", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	w := httptest.NewRecorder()
	a.send(w, r, sender)
	if w.Code != 201 {
		t.Fatalf("send: %d %s", w.Code, w.Body)
	}
	var id int64
	if err := db.QueryRow("SELECT id FROM letters WHERE owner=?", recipient).Scan(&id); err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest("GET", "/api/letters/1/attachments/1", nil)
	r.SetPathValue("id", strconv.FormatInt(id, 10))
	r.SetPathValue("part", "1")
	w = httptest.NewRecorder()
	a.downloadAttachment(w, r, recipient)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("download: %d, %d bytes", w.Code, w.Body.Len())
	}
}
