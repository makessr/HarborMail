package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMySQLMailboxWriteContention(t *testing.T) {
	dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set HARBOR_TEST_MYSQL_DSN to a disposable MySQL database")
	}
	accounts := make(map[string][]byte)
	var owners []string
	for range 6 {
		owner := token()[:16] + "@harbor.test"
		owners = append(owners, owner)
		accounts[owner] = nil
	}
	var instances [2]*app
	for i := range instances {
		db, err := openDB("mysql", dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		instances[i] = &app{db: db, dbDriver: "mysql", domain: "harbor.test", accounts: accounts, sharedMailboxEvents: true}
	}
	first, second := instances[0], instances[1]
	const subject = "Shared database fixture"
	raw := []byte("Subject: " + subject + "\r\n\r\nFixture\r\n")
	deliver := func(a *app, owner string) error {
		session := delivery{app: a, sender: "sender@harbor.test"}
		if err := session.Rcpt(owner, nil); err != nil {
			return err
		}
		return session.Data(strings.NewReader(string(raw)))
	}
	for _, owner := range owners {
		if err := deliver(first, owner); err != nil {
			t.Fatal(err)
		}
	}
	const largeMailboxSize = 2001
	for _, owner := range owners[4:] {
		// Seed a substantial folder without measuring synthetic setup writes.
		for range 2 {
			if _, err := first.db.Exec(`INSERT INTO letters(owner,sender,recipient,subject,body,stamp,folder,origin,raw)
				WITH RECURSIVE fixture(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM fixture WHERE n<1000)
				SELECT ?,?,?,?,'',1,'inbox','inbox',? FROM fixture`, owner, owner, owner, subject, raw); err != nil {
				t.Fatal(err)
			}
		}
		messages, _, _, err := first.imapSnapshot(owner, "inbox")
		if err != nil || len(messages) != largeMailboxSize {
			t.Fatalf("assign legacy UIDs: got %d messages, err=%v", len(messages), err)
		}
	}
	var id int64
	if err := first.db.QueryRow("SELECT id FROM letters WHERE owner=?", owners[0]).Scan(&id); err != nil {
		t.Fatal(err)
	}
	lock, err := first.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if err := lockIMAPUIDs(t.Context(), lock); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	key := mailTranslationKey(owners[0], id, "demo", "en", subject, raw)
	err = second.storeTranslation(ctx, key, subject, raw, translatedEmail{Subject: subject, Body: "translated"})
	if err == nil {
		_, err = second.loadWebPushKeys(ctx)
	}
	if err == nil {
		messages, _, _, readErr := second.imapSnapshotContext(ctx, owners[4], "inbox")
		err = readErr
		if err == nil && len(messages) != largeMailboxSize {
			err = fmt.Errorf("IMAP snapshot returned %d messages, want %d", len(messages), largeMailboxSize)
		}
	}
	cancel()
	if err != nil {
		t.Fatalf("independent operations waited on the IMAP UID lock: %v", err)
	}
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}

	type sample struct {
		operation string
		duration  time.Duration
		err       error
	}
	const iterations = 20
	for _, mode := range []struct {
		name string
		read func(*app, context.Context, string, string) ([]imapMessage, uint32, uint32, error)
	}{
		{"locked", (*app).imapSnapshotWithUIDAssignment},
		{"read-only", (*app).imapSnapshotContext},
	} {
		t.Run(mode.name, func(t *testing.T) {
			start, samples := make(chan struct{}), make(chan sample, iterations*len(owners))
			var workers sync.WaitGroup
			for worker, owner := range owners {
				instance := instances[worker%len(instances)]
				workers.Go(func() {
					<-start
					for range iterations {
						begin := time.Now()
						switch worker / 2 {
						case 0:
							err := deliver(instance, owner)
							samples <- sample{"smtp", time.Since(begin), err}
						case 1:
							request := httptest.NewRequest(http.MethodPost, "/api/drafts",
								strings.NewReader(`{"subject":"Concurrent draft","body":"Synthetic draft"}`))
							request.Header.Set("Content-Type", "application/json")
							response := httptest.NewRecorder()
							instance.createDraft(response, request, owner)
							var err error
							if response.Code != http.StatusCreated {
								err = fmt.Errorf("draft HTTP %d: %s", response.Code, response.Body)
							}
							samples <- sample{"draft", time.Since(begin), err}
						case 2:
							messages, _, _, err := mode.read(instance, t.Context(), owner, "inbox")
							if err == nil && len(messages) != largeMailboxSize {
								err = fmt.Errorf("IMAP snapshot returned %d messages, want %d", len(messages), largeMailboxSize)
							}
							samples <- sample{"imap", time.Since(begin), err}
						}
					}
				})
			}
			before := loadRowLockWaits(first)
			close(start)
			workers.Wait()
			close(samples)
			latencies := make(map[string][]time.Duration)
			for result := range samples {
				if result.err != nil {
					t.Errorf("%s: %v", result.operation, result.err)
				}
				latencies[result.operation] = append(latencies[result.operation], result.duration)
			}
			for _, operation := range []string{"smtp", "draft", "imap"} {
				samples := latencies[operation]
				slices.Sort(samples)
				t.Logf("%s n=%d p95=%v", operation, len(samples), samples[(len(samples)*95+99)/100-1])
			}
			t.Logf("MySQL InnoDB row lock waits: %d", loadRowLockWaits(first)-before)
		})
	}
}
