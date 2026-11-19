package main

import (
	"context"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"
)

func TestMySQLLoginLimitSharedAcrossInstances(t *testing.T) {
	dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set HARBOR_TEST_MYSQL_DSN to a disposable MySQL database")
	}
	var guards [2]webLoginGuard
	for i := range guards {
		db, err := openDB("mysql", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		guards[i].shared, guards[i].scope = &mysqlLoginLimiter{db: db}, "web"
	}
	ctx := context.Background()
	source := netip.MustParsePrefix("192.0.2.10/32")
	email := token() + "@rate.harbor.test"
	results := make(chan time.Duration, loginPairLimit*2)
	var workers sync.WaitGroup
	for i := range cap(results) {
		guard := &guards[i%len(guards)]
		workers.Go(func() { results <- guard.reserve(ctx, source, email, true) })
	}
	workers.Wait()
	close(results)
	allowed := 0
	for retry := range results {
		if retry == 0 {
			allowed++
		} else if retry < loginWindow-time.Minute || retry > loginWindow {
			t.Fatalf("unexpected retry duration: %s", retry)
		}
	}
	if allowed != loginPairLimit {
		t.Fatalf("allowed %d shared reservations, want %d", allowed, loginPairLimit)
	}
	protocol := webLoginGuard{shared: guards[1].shared, scope: "protocol"}
	if retry := protocol.reserve(ctx, source, email, true); retry != 0 {
		t.Fatalf("web limit affected protocol reservations: %s", retry)
	}
	otherSource := netip.MustParsePrefix("192.0.2.11/32")
	if retry := guards[0].reserve(ctx, otherSource, email, true); retry != 0 {
		t.Fatalf("web limit affected a different source: %s", retry)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if retry := guards[0].reserve(canceled, otherSource, email, true); retry <= 0 {
		t.Fatal("reservation succeeded without a database decision")
	}
}
