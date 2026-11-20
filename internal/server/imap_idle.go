package server

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-imap/server"
)

const imapIdleKeepalive = 5 * time.Minute

var (
	errIMAPIdleFolderGone   = errors.New("Selected mailbox no longer exists")
	errIMAPIdleExpectedDone = errors.New("Expected DONE")
)

type imapIdle struct{ commands.Idle }

func (*imapIdle) Parse(fields []interface{}) error {
	if len(fields) != 0 {
		return errors.New("IDLE does not accept arguments")
	}
	return nil
}

func (*imapIdle) Handle(conn server.Conn) error {
	session := conn.Context()
	if session.User == nil {
		return server.ErrNotAuthenticated
	}
	mailbox, _ := session.Mailbox.(*imapMailbox)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var signal <-chan struct{}
	var retrySnapshot bool
	refresh := func() error {
		err := refreshSelectedIMAPContext(ctx, conn)
		retrySnapshot = err != nil
		return err
	}
	if mailbox != nil {
		var unsubscribe func()
		signal, unsubscribe = mailbox.app.subscribeMailbox(ctx, mailbox.owner)
		defer unsubscribe()
		if err := refresh(); err != nil && !errors.Is(err, errIMAPStorage) {
			if errors.Is(err, errIMAPIdleFolderGone) {
				return disconnectIMAPIdle(conn, err.Error())
			}
			return err
		}
	}
	if err := conn.WriteResp(&imap.ContinuationReq{Info: "idling"}); err != nil {
		return err
	}

	// Only the reader goroutine touches the input until DONE. All mailbox
	// snapshots, unsolicited writes and sequence changes stay on this handler.
	read := make(chan error, 1)
	go func() {
		err := readIdleDone(conn)
		cancel()
		read <- err
	}()
	readFinished := false
	defer func() {
		if !readFinished {
			if session.State != imap.LogoutState {
				session.State = imap.LogoutState
				_ = conn.Close()
			}
			<-read
		}
	}()
	keepalive := time.NewTicker(imapIdleKeepalive)
	defer keepalive.Stop()
	var ticks <-chan time.Time
	if mailbox != nil {
		poll := time.NewTicker(mailboxRevisionPoll)
		defer poll.Stop()
		ticks = poll.C
	}
	for {
		var err error
		select {
		case err := <-read:
			readFinished = true
			if err == nil {
				return nil
			}
			if errors.Is(err, errIMAPIdleExpectedDone) {
				return disconnectIMAPIdle(conn, err.Error())
			}
			return disconnectIMAPIdle(conn, "")
		case <-signal:
			signal = mailbox.app.events.watch(mailbox.owner)
			err = refresh()
		case <-ticks:
			if retrySnapshot {
				err = refresh()
			}
		case <-keepalive.C:
			if err := conn.WriteResp(&imap.StatusResp{Type: imap.StatusRespOk, Info: "Still idling"}); err != nil {
				return err
			}
		}
		if errors.Is(err, errIMAPIdleFolderGone) {
			return disconnectIMAPIdle(conn, err.Error())
		}
		if err != nil && !errors.Is(err, errIMAPStorage) && !errors.Is(err, context.Canceled) {
			return err
		}
	}
}

func disconnectIMAPIdle(conn server.Conn, reason string) error {
	if reason != "" {
		_ = conn.WriteResp(&imap.StatusResp{Type: imap.StatusRespBye, Info: reason})
	}
	conn.Context().State = imap.LogoutState
	_ = conn.Close()
	return &imap.ErrStatusResp{}
}

func readIdleDone(reader io.Reader) error {
	var line [len("DONE\r\n")]byte
	for i := range line {
		// Do not create a second buffered reader that could swallow bytes
		// from the next command when DONE and that command arrive together.
		if _, err := io.ReadFull(reader, line[i:i+1]); err != nil {
			return err
		}
		if line[i] == '\n' {
			if strings.EqualFold(strings.TrimSuffix(string(line[:i]), "\r"), "DONE") {
				return nil
			}
			break
		}
	}
	return errIMAPIdleExpectedDone
}
