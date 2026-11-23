package server

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"sync"
	"time"
)

const protocolCheckTimeout = 5 * time.Second

type protocolSession struct {
	owner      string
	generation int64
	appDigest  []byte
}

func (a *app) protocolAccountState(ctx context.Context, owner string) (generation int64, suspended bool, err error) {
	err = a.db.QueryRowContext(ctx, `SELECT protocol_generation,suspended FROM account_access WHERE owner=?`, owner).
		Scan(&generation, &suspended)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

func (a *app) checkProtocolSession(ctx context.Context, session protocolSession) error {
	generation, suspended, err := a.protocolAccountState(ctx, session.owner)
	if err != nil {
		return errLoginUnavailable
	}
	if suspended || generation != session.generation {
		return errLoginInvalid
	}
	if session.appDigest != nil {
		var active bool
		err := a.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM app_passwords
			WHERE owner=? AND digest=? AND (expires=0 OR expires>?))`,
			session.owner, session.appDigest, time.Now().Unix()).Scan(&active)
		if err != nil {
			return errLoginUnavailable
		}
		if !active {
			return errLoginInvalid
		}
	}
	return nil
}

// Subscribe before checking the generation so revocation during login cannot
// be lost when the first shared-mailbox poll establishes its baseline.
func (a *app) watchProtocolSession(ctx context.Context, session protocolSession, closeTransport func() error) {
	signal, unsubscribe := a.subscribeMailbox(ctx, session.owner)
	defer unsubscribe()
	for {
		check, cancel := context.WithTimeout(ctx, protocolCheckTimeout)
		err := a.checkProtocolSession(check, session)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			_ = closeTransport()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-a.stopping:
			return
		case <-signal:
			signal = a.events.watch(session.owner)
		}
	}
}

type imapListener struct {
	net.Listener
	connections sync.Map
}

type imapTransport struct {
	net.Conn
	ctx     context.Context
	cleanup func()
	once    sync.Once
}

func (l *imapListener) Accept() (net.Conn, error) {
	raw, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	peer := raw.RemoteAddr()
	ctx, cancel := context.WithCancel(context.Background())
	conn := &imapTransport{Conn: raw, ctx: ctx, cleanup: func() {
		cancel()
		l.connections.Delete(peer)
	}}
	l.connections.Store(peer, conn)
	return conn, nil
}

func (c *imapTransport) Close() error {
	c.once.Do(c.cleanup)
	return c.Conn.Close()
}
