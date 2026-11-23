package server

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
	pollers map[string]*mailboxPoller
}

type mailboxPoller struct {
	readers int
	ready   chan struct{}
	cancel  context.CancelFunc
}

// One database revision probe per active account and instance, shared by
// Web and IMAP subscribers. The first subscriber establishes the baseline.
func (a *app) subscribeMailbox(ctx context.Context, owner string) (<-chan struct{}, func()) {
	signal := a.events.watch(owner)
	if !a.sharedMailboxEvents {
		return signal, func() {}
	}
	e := &a.events
	e.mu.Lock()
	poller := e.pollers[owner]
	if poller == nil {
		if e.pollers == nil {
			e.pollers = make(map[string]*mailboxPoller)
		}
		background, cancel := context.WithCancel(context.Background())
		poller = &mailboxPoller{ready: make(chan struct{}), cancel: cancel}
		e.pollers[owner] = poller
		go a.pollMailbox(background, owner, poller.ready)
	}
	poller.readers++
	e.mu.Unlock()
	select {
	case <-poller.ready:
	case <-ctx.Done():
	}
	return signal, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		poller.readers--
		if poller.readers == 0 {
			delete(e.pollers, owner)
			poller.cancel()
		}
	}
}

func (a *app) pollMailbox(ctx context.Context, owner string, ready chan<- struct{}) {
	revision, err := a.mailboxRevision(ctx, owner)
	known := err == nil
	close(ready)
	ticker := time.NewTicker(mailboxRevisionPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.stopping:
			return
		case <-ticker.C:
			current, err := a.mailboxRevision(ctx, owner)
			if err == nil && (!known || current != revision) {
				revision, known = current, true
				a.events.notify(owner)
			}
		}
	}
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
	signal, unsubscribe := a.subscribeMailbox(r.Context(), owner)
	defer unsubscribe()
	// Subscribe before rechecking authorization so a concurrent revocation is observed.
	session, err := a.browserSession(r)
	if err != nil {
		loginUnavailable(w)
		return
	}
	if session.owner != owner {
		fail(w, http.StatusUnauthorized, "请登录邮箱")
		return
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
		case <-a.stopping:
			return
		case <-heartbeat.C:
			frame = ": heartbeat\n\n"
		case <-signal:
			signal = a.events.watch(owner)
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
