package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"sync/atomic"
	"time"
)

type mysqlLoginLimiter struct {
	db        *sql.DB
	nextSweep atomic.Int64
}

func (l *mysqlLoginLimiter) reserve(ctx context.Context, scope string, key loginBucketKey) time.Duration {
	limit, dimension := loginSourceLimit, "source"
	if key.perAccount {
		limit, dimension = loginPairLimit, "account"
	}
	bucket := sha256.Sum256([]byte(scope + "\x00" + dimension + "\x00" + key.source.String() + "\x00" + key.login))
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Second
	}
	defer tx.Rollback()
	window := int64(loginWindow / time.Second)
	// The upsert holds this bucket's row lock until the decision has committed.
	// MySQL supplies the clock so workers with different host clocks agree.
	_, err = tx.ExecContext(ctx, `INSERT INTO login_rate_buckets(bucket,attempts,expires)
		VALUES(?,1,UNIX_TIMESTAMP()+?) ON DUPLICATE KEY UPDATE
		attempts=IF(expires<=UNIX_TIMESTAMP(),1,LEAST(attempts+1,?)),
		expires=IF(expires<=UNIX_TIMESTAMP(),UNIX_TIMESTAMP()+?,expires)`,
		bucket[:], window, limit+1, window)
	if err != nil {
		return time.Second
	}
	var attempts int
	var remaining int64
	err = tx.QueryRowContext(ctx, `SELECT attempts,expires-UNIX_TIMESTAMP()
		FROM login_rate_buckets WHERE bucket=?`, bucket[:]).Scan(&attempts, &remaining)
	if err != nil || tx.Commit() != nil {
		return time.Second
	}

	now := time.Now().Unix()
	next := l.nextSweep.Load()
	if now >= next && l.nextSweep.CompareAndSwap(next, now+60) {
		_, _ = l.db.ExecContext(ctx, `DELETE FROM login_rate_buckets
			WHERE expires<=UNIX_TIMESTAMP() ORDER BY expires LIMIT 8192`)
	}
	if attempts > limit {
		return time.Duration(max(remaining, 1)) * time.Second
	}
	return 0
}
