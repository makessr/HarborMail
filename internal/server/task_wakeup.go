package server

import (
	"context"
	"database/sql"
	"sync"
	"time"
)

type taskWakeup struct {
	once sync.Once
	ch   chan struct{}
}

func (s *taskWakeup) channel() chan struct{} {
	s.once.Do(func() { s.ch = make(chan struct{}, 1) })
	return s.ch
}

func (s *taskWakeup) notify() {
	select {
	case s.channel() <- struct{}{}:
	default:
	}
}

func (s *taskWakeup) wait(ctx context.Context, due sql.NullInt64, fallback time.Duration) {
	delay := fallback
	if due.Valid {
		delay = min(delay, max(100*time.Millisecond, time.Until(time.Unix(due.Int64, 0))))
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-s.channel():
	case <-timer.C:
	}
}

func (a *app) taskPollInterval() time.Duration {
	if a.dbDriver == "mysql" || a.sharedMailboxEvents {
		return 10 * time.Second
	}
	return time.Minute
}

func (a *app) commitOutbox(tx *sql.Tx, sender string) error {
	if err := a.commitMailbox(tx, sender); err != nil {
		return err
	}
	a.outboxWakeup.notify()
	return nil
}
