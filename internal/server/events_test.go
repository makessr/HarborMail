package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSharedMailboxEventsDeliverAcrossInstances(t *testing.T) {
	dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set HARBOR_TEST_MYSQL_DSN to a disposable MySQL database")
	}
	readDB, err := openDB("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer readDB.Close()
	writeDB, err := openDB("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer writeDB.Close()

	suffix := token()[:12] + "@events.harbor.test"
	owner, other := "recipient-"+suffix, "other-"+suffix
	accounts := map[string][]byte{owner: nil, other: nil}
	reader := &app{db: readDB, accounts: accounts, sharedMailboxEvents: true}
	writer := &app{db: writeDB, accounts: accounts, sharedMailboxEvents: true}
	web := httptest.NewServer(reader.routes())
	defer web.Close()

	openStream := func(ctx context.Context, account string) *bufio.Reader {
		t.Helper()
		cookie := token()
		digest := sha256.Sum256([]byte(cookie))
		login := httptest.NewRequest(http.MethodPost, "/api/login", nil)
		if err := reader.createBrowserSession(login, account, digest[:], accounts[account], ""); err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, web.URL+"/api/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
		res, err := web.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = res.Body.Close() })
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("unexpected SSE response for %s: %s", account, res.Status)
		}
		stream := bufio.NewReader(res.Body)
		if line, err := stream.ReadString('\n'); err != nil || line != ": connected\n" {
			t.Fatalf("first SSE line = %q, %v", line, err)
		}
		if _, err := stream.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		return stream
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*mailboxRevisionPoll+2*time.Second)
	defer cancel()
	otherCtx, cancelOther := context.WithTimeout(ctx, mailboxRevisionPoll+2*time.Second)
	defer cancelOther()
	foreign := openStream(otherCtx, other)
	mailbox := openStream(ctx, owner)

	// Store an incoming SMTP message on the writer, which has no access to
	// the web server's in-memory notification channel.
	receive := &delivery{app: writer, sender: "sender@external.test"}
	if err := receive.Rcpt(owner, nil); err != nil {
		t.Fatal(err)
	}
	if err := receive.Data(strings.NewReader("Subject: Shared MySQL mailbox\r\n\r\nTest message\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := mailbox.ReadString('\n'); err != nil || line != "event: mailbox\n" {
		t.Fatalf("cross-instance mailbox event = %q, %v", line, err)
	}
	if line, err := mailbox.ReadString('\n'); err != nil || line != "data: changed\n" {
		t.Fatalf("cross-instance mailbox event data = %q, %v", line, err)
	}
	if line, err := foreign.ReadString('\n'); err == nil || otherCtx.Err() != context.DeadlineExceeded {
		t.Fatalf("unrelated mailbox stream returned %q, %v", line, err)
	}
}
