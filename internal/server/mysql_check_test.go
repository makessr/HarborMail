package server

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMySQLCheck(t *testing.T) {
	dsn := os.Getenv("HARBOR_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set HARBOR_TEST_MYSQL_DSN to a disposable MySQL database")
	}
	db, err := openDB("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := checkMySQL(ctx, &out, dsn, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "privileges verified on letters") {
		t.Fatal("expected effective mail table privileges to be checked")
	}

	cfg, err := harborMySQLConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxAllowedPacket = mailLimit
	if err := checkMySQL(ctx, &out, cfg.FormatDSN(), false); err == nil || !strings.Contains(err.Error(), "max_allowed_packet") {
		t.Fatal("expected the packet limit check to fail")
	}
	cfg.MaxAllowedPacket = 0
	if cfg.Params == nil {
		cfg.Params = make(map[string]string)
	}
	cfg.Params["transaction_read_only"] = "ON"
	if err := checkMySQL(ctx, &out, cfg.FormatDSN(), false); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatal("expected the read-only session check to fail")
	}
}
