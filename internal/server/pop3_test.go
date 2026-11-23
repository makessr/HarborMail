package server

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestPOP3SLoginWithLargeLegacyMessage(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testPOP3SLogin(t, "sqlite", filepath.Join(t.TempDir(), "pop.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql", func(t *testing.T) { testPOP3SLogin(t, "mysql", dsn) })
	}
}

func testPOP3SLogin(t *testing.T, driver, source string) {
	db, err := openDB(driver, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := token()[:16] + "@harbor.test"
	const password = "synthetic-pop3-password"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, dbDriver: driver, accounts: map[string][]byte{owner: hash}}
	raw := []byte("Subject: fixture\nMIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=fixture\n\n" +
		"--fixture\nContent-Type: application/octet-stream\nContent-Disposition: attachment; filename=large.bin\n" +
		"Content-Transfer-Encoding: base64\n\n" + strings.Repeat(strings.Repeat("YQ==", 19)+"\n", 20000) + "--fixture--\n")
	expected := bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n"))
	result, err := db.Exec(`INSERT INTO letters(owner,sender,recipient,subject,body,stamp,folder,origin,raw)
		VALUES(?,?,?,'fixture','',1,'inbox','inbox',?)`, owner, owner, owner, raw)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DELETE FROM letters WHERE id=?", id)
	before, err := a.popSnapshot(owner)
	if err != nil || len(before) != 1 || before[0].size != int64(len(expected)) {
		t.Fatalf("legacy snapshot = %+v, %v", before, err)
	}
	if err := a.backfillPOPOctets(t.Context()); err != nil {
		t.Fatal(err)
	}
	var stored int
	if err := db.QueryRow("SELECT pop_octets FROM letters WHERE id=?", id).Scan(&stored); err != nil || stored != len(expected) {
		t.Fatalf("stored octets = %d, %v", stored, err)
	}

	now := time.Now()
	cert, key := makeReloadTestPEM(t, 1, now.Add(-time.Hour), now.Add(time.Hour))
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := popServer{app: a, slots: make(chan struct{}, 1)}
	server.slots <- struct{}{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err == nil {
			server.serve(conn.(*tls.Conn))
		}
	}()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(cert)
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", listener.Addr().String(), &tls.Config{RootCAs: roots})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	client := textproto.NewConn(conn)
	expect := func(command, response string) {
		t.Helper()
		if command != "" {
			if err := client.PrintfLine("%s", command); err != nil {
				t.Fatal(err)
			}
		}
		if line, err := client.ReadLine(); line != response || err != nil {
			t.Fatalf("%s: wanted %q, got %q, %v", strings.Fields(command + " greeting")[0], response, line, err)
		}
	}
	expect("", "+OK Harbor Mail ready")
	expect("USER "+owner, "+OK Username accepted")
	expect("PASS "+password, "+OK Maildrop ready")
	expect("STAT", fmt.Sprintf("+OK 1 %d", stored))
	expect("LIST 1", fmt.Sprintf("+OK 1 %d", stored))
	expect("RETR 1", fmt.Sprintf("+OK %d octets", stored))
	downloaded, err := client.ReadDotBytes()
	if err != nil || !bytes.Equal(downloaded, raw) {
		t.Fatalf("RETR: got %d decoded bytes, %v", len(downloaded), err)
	}
	expect("QUIT", "+OK Goodbye")
	<-done
}

func TestPOPMultiline(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		data string
		size int64
	}{
		{"empty", "", "", 0},
		{"escaped dot", ".foo\r\n.\n", "..foo\r\n..\r\n", 9},
		{"no newline", "abc", "abc\r\n", 5},
		{"ends in carriage return", "abc\r", "abc\r\n", 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			var response bytes.Buffer
			session := popSession{writer: textproto.NewWriter(bufio.NewWriter(&response))}
			if err := session.multiline("Message", []byte(test.raw)); err != nil {
				t.Fatal(err)
			}
			if got, want := response.String(), "+OK Message\r\n"+test.data+".\r\n"; got != want {
				t.Errorf("wire response = %q, want %q", got, want)
			}
			if got := popOctets([]byte(test.raw)); got != test.size {
				t.Errorf("octets = %d, want %d", got, test.size)
			}
		})
	}
}
