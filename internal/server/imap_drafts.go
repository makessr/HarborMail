package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/mail"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-imap/server"
)

type imapAppend struct{ commands.Append }

func (h *imapAppend) Handle(conn server.Conn) error {
	ctx := conn.Context()
	if ctx.User == nil {
		return server.ErrNotAuthenticated
	}
	destination, err := ctx.User.GetMailbox(h.Mailbox)
	if err != nil {
		return err
	}
	selected := ctx.Mailbox != nil && ctx.Mailbox.Name() == destination.Name()
	if selected && ctx.MailboxReadOnly {
		return server.ErrMailboxReadOnly
	}
	if err := destination.CreateMessage(h.Flags, h.Date, h.Message); err != nil {
		return err
	}
	if selected {
		// The draft is durable. If refresh fails, NOOP can retry without a second APPEND.
		_ = refreshSelectedIMAP(conn)
	}
	return nil
}

func (m *imapMailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	if m.folder != "drafts" {
		return errIMAPReadOnly
	}
	raw, err := io.ReadAll(io.LimitReader(body, int64(m.app.messageLimit())+1))
	if err != nil || len(raw) == 0 || len(raw) > m.app.messageLimit() {
		return fmt.Errorf("draft must be between 1 byte and %d MiB", m.app.messageLimit()>>20)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return errors.New("Draft MIME headers could not be read")
	}
	recipient := ""
	if first, err := parsed.Header.AddressList("To"); err == nil && len(first) > 0 && len(first[0].Address) <= 254 {
		recipient = first[0].Address
	}
	draft := interpret(raw, m.owner, recipient)
	draft.From = m.owner
	if !date.IsZero() {
		draft.Date = date.Unix()
	}
	for _, flag := range flags {
		switch imap.CanonicalFlag(flag) {
		case imap.SeenFlag:
			draft.Read = true
		case imap.FlaggedFlag:
			draft.Star = true
		case imap.DraftFlag:
		default:
			return errors.New("Draft APPEND supports Seen, Flagged, and Draft")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := m.app.db.BeginTx(ctx, nil)
	if err != nil {
		log.Print("IMAP draft transaction: ", err)
		return errIMAPStorage
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return errIMAPStorage
	}
	if err := m.app.requireActiveAccount(ctx, tx, m.owner); err != nil {
		if errors.Is(err, errAccountSuspended) {
			return errors.New("Mailbox access is suspended")
		}
		return errIMAPStorage
	}
	if err := insert(tx, m.owner, draft, "drafts", raw); err != nil {
		if isQuotaExceeded(err) {
			return errors.New("[OVERQUOTA] Mailbox storage quota exceeded")
		}
		log.Print("IMAP draft append: ", err)
		return errIMAPStorage
	}
	if err := m.app.commitMailbox(tx, m.owner); err != nil {
		log.Print("IMAP draft commit: ", err)
		return errIMAPStorage
	}
	return nil
}

func (m *imapMailbox) rawIMAPMessage(msg imapMessage) ([]byte, error) {
	var raw []byte
	err := m.app.db.QueryRow(`SELECT l.raw FROM letters l JOIN imap_uids u ON u.letter_id=l.id AND u.folder=l.folder
		WHERE l.owner=? AND l.id=? AND l.folder=? AND u.uid=?`, m.owner, msg.id, m.folder, msg.Uid).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errIMAPMailboxChanged
	}
	return raw, err
}
