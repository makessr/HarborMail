package server

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/server"
	"golang.org/x/crypto/bcrypt"
)

func TestAdminRevokesProtocolSessions(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		testAdminRevokesProtocolSessions(t, "sqlite", filepath.Join(t.TempDir(), "mail.db"))
	})
	if dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN"); dsn != "" {
		t.Run("mysql-two-instances", func(t *testing.T) {
			testAdminRevokesProtocolSessions(t, "mysql", dsn)
		})
	}
}

func testAdminRevokesProtocolSessions(t *testing.T, driver, source string) {
	hash, err := bcrypt.GenerateFromPassword([]byte("synthetic-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	owner := token()[:12] + "@harbor.test"
	admin := token()[:12] + "@harbor.test"
	accounts := map[string][]byte{owner: hash, admin: hash}
	newInstance := func() *app {
		db, err := openDB(driver, source)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return &app{db: db, dbDriver: driver, sharedMailboxEvents: driver == "mysql",
			accounts: accounts, admins: map[string]bool{admin: true}}
	}
	first := newInstance()
	instances := []*app{first}
	if driver == "mysql" {
		instances = append(instances, newInstance())
	}
	routes := first.routes()
	login := httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"email":"`+admin+`","password":"synthetic-password"}`))
	login.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, login)
	cookies := recorder.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("admin login: %d %s", recorder.Code, recorder.Body)
	}
	adminAction := func(scope string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/"+owner+"/access",
			strings.NewReader(`{"scope":"`+scope+`","password":"synthetic-password"}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookies[0])
		recorder := httptest.NewRecorder()
		routes.ServeHTTP(recorder, req)
		if recorder.Code != 200 {
			t.Fatalf("%s: %d %s", scope, recorder.Code, recorder.Body)
		}
	}

	now := time.Now()
	pem, key := makeReloadTestPEM(t, 1, now.Add(-time.Hour), now.Add(time.Hour))
	pair, err := tls.X509KeyPair(pem, key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	serverTLS := &tls.Config{Certificates: []tls.Certificate{pair}}
	clientTLS := &tls.Config{RootCAs: roots}
	type endpoints struct{ imap, pop string }
	addresses := make([]endpoints, 0, len(instances))
	for _, instance := range instances {
		tcp, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		tracked := &imapListener{Listener: tcp}
		imap := server.New(imapBackend{app: instance, listener: tracked})
		enableHarborIMAPCommands(imap)
		t.Cleanup(func() { _ = tcp.Close(); _ = imap.Close() })
		go func() { _ = imap.Serve(tls.NewListener(tracked, serverTLS)) }()

		popListener, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = popListener.Close() })
		pop := &popServer{app: instance, slots: make(chan struct{}, 8)}
		go func() {
			for {
				transport, err := popListener.Accept()
				if err != nil {
					return
				}
				pop.slots <- struct{}{}
				go pop.serve(transport.(*tls.Conn))
			}
		}()
		addresses = append(addresses, endpoints{tcp.Addr().String(), popListener.Addr().String()})
	}
	dial := func(addr string) *textproto.Conn {
		t.Helper()
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr, clientTLS)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := conn.SetDeadline(time.Now().Add(2*mailboxRevisionPoll + 5*time.Second)); err != nil {
			t.Fatal(err)
		}
		return textproto.NewConn(conn)
	}
	expect := func(client *textproto.Conn, command, prefix string) {
		t.Helper()
		if command != "" {
			if err := client.PrintfLine("%s", command); err != nil {
				t.Fatal(err)
			}
		}
		for {
			line, err := client.ReadLine()
			if err != nil {
				t.Fatalf("waiting for %q: %v", prefix, err)
			}
			if strings.HasPrefix(line, prefix) {
				return
			}
			if strings.HasPrefix(line, "-ERR") || strings.Contains(line, " NO ") {
				t.Fatalf("waiting for %q: %s", prefix, line)
			}
		}
	}
	logIn := func(password string) []*textproto.Conn {
		t.Helper()
		var clients []*textproto.Conn
		for _, endpoints := range addresses {
			imap := dial(endpoints.imap)
			expect(imap, "", "* OK")
			expect(imap, "A1 LOGIN "+owner+" "+password, "A1 OK")
			expect(imap, "A2 SELECT INBOX", "A2 OK")
			expect(imap, "A3 IDLE", "+ idling")
			pop := dial(endpoints.pop)
			expect(pop, "", "+OK")
			expect(pop, "USER "+owner, "+OK")
			expect(pop, "PASS "+password, "+OK")
			clients = append(clients, imap, pop)
		}
		return clients
	}
	expectClosed := func(scope string, clients []*textproto.Conn) {
		t.Helper()
		for _, client := range clients {
			for {
				_, err := client.ReadLine()
				if err == nil {
					continue
				}
				if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("%s did not close connection: %v", scope, err)
				}
				_ = client.Close()
				break
			}
		}
	}
	for _, scope := range []string{"sessions", "suspend"} {
		clients := logIn("synthetic-password")
		adminAction(scope)
		expectClosed(scope, clients)
	}
	adminAction("restore")
	password := appPasswordPrefix + token()
	digest := sha256.Sum256([]byte(password))
	credential, err := first.baseAccountDigest(t.Context(), first.db, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.db.Exec(`INSERT INTO app_passwords(owner,digest,credential,label,protocols,created,expires)
		VALUES(?,?,?,'test',?,?,0)`, owner, digest[:], credential[:], clientIMAP|clientPOP3, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	clients := logIn(password)
	adminAction("app_passwords")
	expectClosed("app_passwords", clients)
	for _, client := range logIn("synthetic-password") {
		_ = client.Close()
	}
}
