package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/backendutil"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/responses"
	"github.com/emersion/go-imap/server"
	"github.com/emersion/go-message"
)

func (a *app) listenIMAPS(runtime *serviceRuntime, addr string, config *tls.Config) error {
	if addr == "" {
		return nil
	}
	if config == nil {
		return errors.New("IMAPS requires TLS certificate configuration")
	}
	listener, err := tls.Listen("tcp", addr, config.Clone())
	if err != nil {
		return fmt.Errorf("listen for IMAPS: %w", err)
	}
	service := server.New(imapBackend{app: a})
	enableHarborIMAPCommands(service)
	service.AutoLogout = server.MinAutoLogout
	service.MaxLiteralSize = mailLimit
	log.Printf("IMAPS listening on %s", listener.Addr())
	done := runtime.serve("IMAPS", func() error { return service.Serve(listener) })
	runtime.stops = append(runtime.stops, func(context.Context) {
		_ = listener.Close()
		<-done
		_ = service.Close()
	})
	return nil
}

type imapBackend struct{ app *app }

func (b imapBackend) Login(info *imap.ConnInfo, email, password string) (backend.User, error) {
	var peer net.Addr
	if info != nil {
		peer = info.RemoteAddr
	}
	owner, err := b.app.authenticateProtocol(peer, email, password, clientIMAP)
	if errors.Is(err, errLoginInvalid) {
		return nil, backend.ErrInvalidCredentials
	}
	if err != nil {
		return nil, err
	}
	return &imapUser{app: b.app, owner: owner}, nil
}

var (
	errIMAPReadOnly = errors.New("This operation is available in Harbor Mail Webmail")
	errIMAPStorage  = errors.New("Temporary mailbox storage error")
)

var imapFolders = []struct{ name, folder, attribute string }{
	{"INBOX", "inbox", ""},
	{"Sent", "sent", "\\Sent"},
	{"Drafts", "drafts", "\\Drafts"},
	{"Archive", "archive", "\\Archive"},
	{"Junk", "junk", "\\Junk"},
	{"Trash", "trash", "\\Trash"},
}

type imapUser struct {
	app               *app
	owner             string
	selectingReadOnly bool
}

func (u imapUser) Username() string { return u.owner }
func (u imapUser) Logout() error    { return nil }
func (u imapUser) ListMailboxes(subscribed bool) ([]backend.Mailbox, error) {
	custom, err := listCustomFolders(context.Background(), u.app.db, u.owner)
	if err != nil {
		return nil, imapFolderError(err)
	}
	children := make(map[string]bool)
	subscribedAncestors := make(map[string]bool)
	for _, folder := range custom {
		parent := parentFolderPath(strings.ToLower(folder.Label))
		if parent != "" {
			children[parent] = true
		}
		if subscribed && folder.Subscribed {
			for path := parent; path != ""; path = parentFolderPath(path) {
				subscribedAncestors[path] = true
			}
		}
	}
	result := make([]backend.Mailbox, 0, len(imapFolders)+len(custom))
	for _, folder := range imapFolders {
		result = append(result, &imapMailbox{imapUser: u, name: folder.name, folder: folder.folder,
			attribute: folder.attribute, hasChildren: children[strings.ToLower(folder.name)]})
	}
	for _, folder := range custom {
		path := strings.ToLower(folder.Label)
		ancestor := subscribed && !folder.Subscribed && subscribedAncestors[path]
		if !subscribed || folder.Subscribed || ancestor {
			result = append(result, &imapMailbox{imapUser: u, name: folder.Label, folder: folder.ID,
				hasChildren: children[path], subscriptionAncestor: ancestor})
		}
	}
	return result, nil
}

func (u imapUser) GetMailbox(name string) (backend.Mailbox, error) {
	for _, folder := range imapFolders {
		if strings.EqualFold(name, folder.name) {
			return &imapMailbox{imapUser: u, name: folder.name, folder: folder.folder,
				attribute: folder.attribute, readOnly: u.selectingReadOnly}, nil
		}
	}
	folder, err := u.getCustomByName(name)
	if err != nil {
		return nil, imapFolderError(err)
	}
	return &imapMailbox{imapUser: u, name: folder.Label, folder: folder.ID, readOnly: u.selectingReadOnly}, nil
}

type imapMessage struct {
	id int64
	memory.Message
}

type imapMailbox struct {
	imapUser
	name, folder, attribute string
	hasChildren             bool
	subscriptionAncestor    bool
	messages                []imapMessage
	readOnly                bool
	writeResponse           func(imap.WriterTo) error
}

func (m *imapMailbox) Name() string { return m.name }

func (m *imapMailbox) Info() (*imap.MailboxInfo, error) {
	children := imap.HasNoChildrenAttr
	if m.hasChildren {
		children = imap.HasChildrenAttr
	}
	info := &imap.MailboxInfo{Name: m.name, Delimiter: "/", Attributes: []string{children}}
	if m.subscriptionAncestor {
		info.Attributes = append(info.Attributes, imap.NoSelectAttr)
	}
	if m.attribute != "" {
		info.Attributes = append(info.Attributes, m.attribute)
	}
	return info, nil
}

func (m *imapMailbox) Check() error { return nil }
func (m *imapMailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	messages, validity, next, err := m.app.imapSnapshot(m.owner, m.folder)
	if err != nil {
		log.Print("IMAP mailbox snapshot: ", err)
		return nil, errIMAPStorage
	}
	m.messages = messages
	status := imap.NewMailboxStatus(m.name, items)
	status.ReadOnly = m.readOnly
	status.Flags = []string{imap.SeenFlag, imap.FlaggedFlag}
	if m.folder != "trash" {
		status.Flags = append(status.Flags, imap.DeletedFlag)
	}
	status.PermanentFlags = status.Flags
	if m.folder == "drafts" {
		status.Flags = append(status.Flags, imap.DraftFlag)
	}
	if m.readOnly {
		status.PermanentFlags = []string{}
	}
	status.Messages = uint32(len(messages))
	status.UidValidity = validity
	status.UidNext = next
	for i, msg := range messages {
		seen := false
		for _, flag := range msg.Flags {
			if flag == imap.SeenFlag {
				seen = true
				break
			}
		}
		if !seen {
			status.Unseen++
			if status.UnseenSeqNum == 0 {
				status.UnseenSeqNum = uint32(i + 1)
			}
		}
	}
	return status, nil
}

func (a *app) imapSnapshot(owner, folder string) ([]imapMessage, uint32, uint32, error) {
	return a.imapSnapshotContext(context.Background(), owner, folder)
}

func (a *app) imapSnapshotContext(ctx context.Context, owner, folder string) (messages []imapMessage, validity, next uint32, err error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return nil, 0, 0, err
	}
	if err := verifyFolder(ctx, tx, owner, folder); err != nil {
		return nil, 0, 0, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT validity, next_uid FROM imap_uid_state WHERE id=1`).Scan(&validity, &next); err != nil {
		return nil, 0, 0, err
	}
	initialNext := next
	if err := assignMailboxUIDs(ctx, tx, owner, folder, &next); err != nil {
		return nil, 0, 0, err
	}
	if next != initialNext {
		if _, err := tx.ExecContext(ctx, `UPDATE imap_uid_state SET next_uid=? WHERE id=1`, next); err != nil {
			return nil, 0, 0, err
		}
	}
	messages, err = queryIMAPMessages(ctx, tx, owner, folder)
	if err != nil {
		return nil, 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, 0, err
	}
	return messages, validity, next, nil
}

func queryIMAPMessages(ctx context.Context, tx *sql.Tx, owner, folder string) ([]imapMessage, error) {
	rows, err := tx.QueryContext(ctx, `SELECT l.id,l.stamp,l.seen,l.star,length(l.raw),u.uid,
		CASE WHEN d.uid IS NULL THEN 0 ELSE 1 END
		FROM letters l JOIN imap_uids u ON u.letter_id=l.id AND u.folder=l.folder
		LEFT JOIN imap_deleted d ON d.letter_id=l.id AND d.uid=u.uid
		WHERE l.owner=? AND l.folder=? ORDER BY u.uid`, owner, folder)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []imapMessage
	for rows.Next() {
		var msg imapMessage
		var stamp int64
		var seen, star, deleted bool
		if err := rows.Scan(&msg.id, &stamp, &seen, &star, &msg.Size, &msg.Uid, &deleted); err != nil {
			return nil, err
		}
		msg.Date = time.Unix(stamp, 0).UTC()
		msg.Flags = mailboxFlags(seen, star, deleted, folder == "drafts")
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}

func assignMailboxUIDs(ctx context.Context, tx *sql.Tx, owner, folder string, next *uint32) error {
	rows, err := tx.QueryContext(ctx, `SELECT l.id FROM letters l LEFT JOIN imap_uids u
		ON u.letter_id=l.id AND u.folder=l.folder WHERE l.owner=? AND l.folder=? AND u.uid IS NULL ORDER BY l.id`,
		owner, folder)
	if err != nil {
		return err
	}
	defer rows.Close()
	var missing []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		missing = append(missing, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if uint64(*next)+uint64(len(missing)) > math.MaxUint32 {
		return errors.New("IMAP UID space exhausted")
	}
	for _, id := range missing {
		if _, err := tx.ExecContext(ctx, `DELETE FROM imap_deleted WHERE letter_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM imap_uids WHERE letter_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO imap_uids (letter_id,uid,folder) VALUES(?,?,?)`, id, *next, folder); err != nil {
			return err
		}
		*next += 1
	}
	return nil
}

func (m *imapMailbox) maxUID() uint32 {
	if len(m.messages) == 0 {
		return 0
	}
	return m.messages[len(m.messages)-1].Uid
}

func imapSequence(set *imap.SeqSet, highest uint32) *imap.SeqSet {
	if set == nil {
		return nil
	}
	result := new(imap.SeqSet)
	for _, seq := range set.Set {
		if seq.Start == 0 {
			seq.Start = highest
		}
		if seq.Stop == 0 {
			seq.Stop = highest
		}
		if seq.Start != 0 && seq.Stop != 0 {
			result.AddRange(seq.Start, seq.Stop)
		}
	}
	return result
}

func (m *imapMailbox) ListMessages(uid bool, set *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	defer close(ch)
	maximum := uint32(len(m.messages))
	if uid {
		maximum = m.maxUID()
	}
	set = imapSequence(set, maximum)
	rawNeeded := false
	for _, item := range items {
		switch item {
		case imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchUid:
		default:
			rawNeeded = true
		}
	}
	for i, msg := range m.messages {
		seq := uint32(i + 1)
		number := seq
		if uid {
			number = msg.Uid
		}
		if !set.Contains(number) {
			continue
		}
		if rawNeeded {
			var err error
			msg.Body, err = m.rawIMAPMessage(msg)
			if err != nil {
				if errors.Is(err, errIMAPMailboxChanged) {
					return err
				}
				log.Print("IMAP retrieve: ", err)
				return errIMAPStorage
			}
		}
		fetched, err := msg.Fetch(seq, items)
		if err != nil {
			return errors.New("Cannot parse stored MIME headers")
		}
		if !m.readOnly && readsMessage(fetched) {
			seen := true
			prior := m.messages[i].Flags
			err := m.persistFlags([]int{i}, &seen, nil, nil)
			if err != nil && !errors.Is(err, errIMAPMailboxChanged) {
				return err
			}
			current := m.messages[i].Flags
			if !slices.Equal(prior, current) {
				fetched.Items[imap.FetchFlags] = nil
				fetched.Flags = current
			}
		}
		ch <- fetched
	}
	return nil
}

func (m *imapMailbox) SearchMessages(uid bool, criteria *imap.SearchCriteria) ([]uint32, error) {
	needsBody := prepareIMAPSearch(criteria, uint32(len(m.messages)), m.maxUID())
	var result []uint32
	for i, msg := range m.messages {
		seq := uint32(i + 1)
		var entity *message.Entity
		if needsBody {
			raw, err := m.rawIMAPMessage(msg)
			if err != nil {
				if errors.Is(err, errIMAPMailboxChanged) {
					return nil, err
				}
				log.Print("IMAP search: ", err)
				return nil, errIMAPStorage
			}
			entity, _ = message.Read(bytes.NewReader(raw))
		}
		if entity == nil {
			entity, _ = message.New(message.Header{}, bytes.NewReader(nil))
		}
		match, err := backendutil.Match(entity, seq, msg.Uid, msg.Date, msg.Flags, criteria)
		if err != nil || !match {
			continue
		}
		if uid {
			result = append(result, msg.Uid)
		} else {
			result = append(result, seq)
		}
	}
	return result, nil
}

// Refresh at command boundaries or during IDLE so sequence numbers stay stable mid-FETCH.
type imapRefreshCommands struct{ idleReady bool }

func enableHarborIMAPCommands(service *server.Server) {
	commands := &imapRefreshCommands{}
	// go-imap skips an entire extension that advertises a built-in IDLE
	// handler during registration. Activate Harbor's IDLE after registration,
	// before the server starts accepting connections.
	service.Enable(commands)
	commands.idleReady = true
}

func (imapRefreshCommands) Capabilities(server.Conn) []string { return nil }

func (extension imapRefreshCommands) Command(name string) server.HandlerFactory {
	switch name {
	case "NOOP":
		return func() server.Handler { return &imapNoop{} }
	case "IDLE":
		if extension.idleReady {
			return func() server.Handler { return &imapIdle{} }
		}
	case "CLOSE":
		return func() server.Handler { return &imapClose{} }
	case "APPEND":
		return func() server.Handler { return &imapAppend{} }
	case "LIST", "LSUB":
		return func() server.Handler {
			handler := &imapList{}
			handler.Subscribed = name == "LSUB"
			return handler
		}
	case "SELECT", "EXAMINE":
		return func() server.Handler {
			handler := &imapSelect{}
			handler.ReadOnly = name == "EXAMINE"
			return handler
		}
	}
	return nil
}

type imapNoop struct{ server.Noop }

func (*imapNoop) Handle(conn server.Conn) error { return refreshSelectedIMAP(conn) }

type imapClose struct{ server.Close }

func (h *imapClose) Handle(conn server.Conn) error {
	if mailbox, ok := conn.Context().Mailbox.(*imapMailbox); ok {
		// CLOSE applies deletion marks without sending untagged EXPUNGE responses.
		mailbox.writeResponse = nil
	}
	return h.Close.Handle(conn)
}

type imapSelect struct{ server.Select }

func (h *imapSelect) Handle(conn server.Conn) error {
	if user, ok := conn.Context().User.(*imapUser); ok {
		user.selectingReadOnly = h.ReadOnly
		defer func() { user.selectingReadOnly = false }()
	}
	err := h.Select.Handle(conn)
	if mailbox, ok := conn.Context().Mailbox.(*imapMailbox); ok {
		mailbox.writeResponse = conn.WriteResp
	}
	return err
}

func refreshSelectedIMAP(conn server.Conn) error {
	return refreshSelectedIMAPContext(context.Background(), conn)
}

func refreshSelectedIMAPContext(ctx context.Context, conn server.Conn) error {
	mailbox, ok := conn.Context().Mailbox.(*imapMailbox)
	if !ok {
		return nil
	}
	fresh, _, _, err := mailbox.app.imapSnapshotContext(ctx, mailbox.owner, mailbox.folder)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, sql.ErrNoRows) && customFolderID.MatchString(mailbox.folder) {
		return errIMAPIdleFolderGone
	}
	if err != nil {
		log.Print("IMAP refresh: ", err)
		return errIMAPStorage
	}
	return mailbox.applyIMAPSnapshot(fresh)
}

func (mailbox *imapMailbox) applyIMAPSnapshot(fresh []imapMessage) error {
	present := make(map[uint32]bool, len(fresh))
	for _, msg := range fresh {
		present[msg.Uid] = true
	}
	oldFlags := make(map[uint32][]string, len(mailbox.messages))
	for i := len(mailbox.messages) - 1; i >= 0; i-- {
		msg := mailbox.messages[i]
		if present[msg.Uid] {
			oldFlags[msg.Uid] = msg.Flags
			continue
		}
		if err := mailbox.reportCount(uint32(i+1), "EXPUNGE"); err != nil {
			return err
		}
	}
	if len(oldFlags) != len(fresh) {
		if err := mailbox.reportCount(uint32(len(fresh)), "EXISTS"); err != nil {
			return err
		}
	}
	for i, msg := range fresh {
		old, found := oldFlags[msg.Uid]
		if !found || slices.Equal(old, msg.Flags) || mailbox.writeResponse == nil {
			continue
		}
		msgs := make(chan *imap.Message, 1)
		update := imap.NewMessage(uint32(i+1), []imap.FetchItem{imap.FetchFlags, imap.FetchUid})
		update.Flags, update.Uid = msg.Flags, msg.Uid
		msgs <- update
		close(msgs)
		if err := mailbox.writeResponse(&responses.Fetch{Messages: msgs}); err != nil {
			return err
		}
	}
	mailbox.messages = fresh
	return nil
}

func prepareIMAPSearch(c *imap.SearchCriteria, highestSeq, highestUID uint32) bool {
	c.SeqNum = imapSequence(c.SeqNum, highestSeq)
	c.Uid = imapSequence(c.Uid, highestUID)
	read := len(c.Header)+len(c.Body)+len(c.Text) > 0 || !c.SentSince.IsZero() ||
		!c.SentBefore.IsZero() || c.Larger != 0 || c.Smaller != 0
	for _, other := range c.Not {
		read = prepareIMAPSearch(other, highestSeq, highestUID) || read
	}
	for _, pair := range c.Or {
		for _, other := range pair {
			read = prepareIMAPSearch(other, highestSeq, highestUID) || read
		}
	}
	return read
}
