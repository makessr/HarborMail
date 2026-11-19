package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"
)

const mailboxRevisionPoll = 5 * time.Second

// Subscribers share a wake-up channel per account. They rearm before reading
// the database so a concurrent commit can trigger another refresh.
type mailboxEvents struct {
	mu      sync.Mutex
	pending map[string]chan struct{}
}

func (e *mailboxEvents) watch(owner string) <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending == nil {
		e.pending = make(map[string]chan struct{})
	}
	signal := e.pending[owner]
	if signal == nil {
		signal = make(chan struct{})
		e.pending[owner] = signal
	}
	return signal
}

func (e *mailboxEvents) notify(owners ...string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, owner := range owners {
		if signal := e.pending[owner]; signal != nil {
			delete(e.pending, owner)
			close(signal)
		}
	}
}

func (a *app) commitMailbox(tx *sql.Tx, owners ...string) error {
	if a.sharedMailboxEvents {
		ordered := slices.Clone(owners)
		slices.Sort(ordered)
		for _, owner := range slices.Compact(ordered) {
			if owner == "" {
				continue
			}
			if _, err := tx.Exec(`INSERT INTO mailbox_revisions(owner,revision) VALUES(?,1)
				ON DUPLICATE KEY UPDATE revision=revision+1`, owner); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.events.notify(owners...)
	return nil
}

func (a *app) mailboxRevision(ctx context.Context, owner string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var revision int64
	err := a.db.QueryRowContext(ctx, `SELECT revision FROM mailbox_revisions WHERE owner=?`, owner).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}

func (a *app) streamMailboxEvents(w http.ResponseWriter, r *http.Request, owner string) {
	signal := a.events.watch(owner)
	// Subscribe before rechecking authorization so a concurrent revocation is observed.
	session, err := a.browserSession(r)
	if err != nil {
		fail(w, http.StatusInternalServerError, "无法读取登录会话")
		return
	}
	if session.owner != owner {
		fail(w, http.StatusUnauthorized, "请登录邮箱")
		return
	}
	var ticks <-chan time.Time
	var revision int64
	var revisionKnown bool
	if a.sharedMailboxEvents {
		revision, err = a.mailboxRevision(r.Context(), owner)
		revisionKnown = err == nil
		poll := time.NewTicker(mailboxRevisionPoll)
		defer poll.Stop()
		ticks = poll.C
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	send := func(frame string) bool {
		// Use a fresh per-write deadline for this long-lived HTTP request.
		if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return false
		}
		_, err := io.WriteString(w, frame)
		return err == nil && controller.Flush() == nil && controller.SetWriteDeadline(time.Time{}) == nil
	}
	if !send(": connected\n\n") {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		var frame string
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			frame = ": heartbeat\n\n"
		case <-signal:
			signal = a.events.watch(owner)
			if a.sharedMailboxEvents {
				if current, err := a.mailboxRevision(r.Context(), owner); err == nil {
					revision, revisionKnown = current, true
				}
			}
			frame = "event: mailbox\ndata: changed\n\n"
		case <-ticks:
			current, err := a.mailboxRevision(r.Context(), owner)
			if err != nil || (revisionKnown && current == revision) {
				continue
			}
			revision, revisionKnown = current, true
			frame = "event: mailbox\ndata: changed\n\n"
		}
		current, err := a.browserSession(r)
		if err != nil {
			return
		}
		if current.owner != owner {
			send("event: session-expired\ndata: expired\n\n")
			return
		}
		if !send(frame) {
			return
		}
	}
}
