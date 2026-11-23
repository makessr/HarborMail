// Package mailqueue drives durable outbound delivery work.
package mailqueue

import (
	"context"
	"database/sql"
	"log"
	"time"
)

// Store owns lease recovery and atomic claims. A nil job means no work is ready.
type Store[Job any] interface {
	Recover(context.Context) error
	Claim() (*Job, error)
	NextDeadline(ctx context.Context, claimAt, recoveryAt int64) (sql.NullInt64, error)
}

func Run[Job any](ctx context.Context, store Store[Job], deliver func(*Job) error, wait func(context.Context, sql.NullInt64, time.Duration), poll time.Duration) {
	for ctx.Err() == nil {
		recoveryAt := time.Now().Unix()
		claimAt := recoveryAt
		retry := false
		if err := store.Recover(ctx); err != nil {
			retry = true
			if ctx.Err() == nil {
				log.Print("outbox recovery: ", err)
			}
		} else {
			for ctx.Err() == nil {
				claimAt = time.Now().Unix()
				job, err := store.Claim()
				if err != nil {
					retry = true
					log.Print("outbox claim: ", err)
					break
				}
				if job == nil {
					break
				}
				if err := deliver(job); err != nil {
					log.Print("outbox delivery state: ", err)
				}
			}
		}
		var next sql.NullInt64
		if !retry && ctx.Err() == nil {
			probe, cancel := context.WithTimeout(ctx, 3*time.Second)
			deadline, err := store.NextDeadline(probe, claimAt, recoveryAt)
			cancel()
			next, retry = deadline, err != nil
		}
		delay := poll
		if retry {
			delay = 3 * time.Second
		}
		wait(ctx, next, delay)
	}
}
