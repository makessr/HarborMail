package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/responses"
	"github.com/emersion/go-imap/server"
)

func imapFolderError(err error) error {
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
		return backend.ErrNoSuchMailbox
	case errors.Is(err, errFolderName), errors.Is(err, errFolderExists), errors.Is(err, errFolderNotEmpty),
		errors.Is(err, errFolderHasChildren), errors.Is(err, errFolderIntoChild), errors.Is(err, errFolderUsedByRule):
		return err
	default:
		log.Print("IMAP folder operation: ", err)
		return errIMAPStorage
	}
	return nil
}

func (u imapUser) getCustomByName(name string) (customFolder, error) {
	label, err := folderName(name)
	if err != nil {
		return customFolder{}, sql.ErrNoRows
	}
	var folder customFolder
	err = u.app.db.QueryRow(`SELECT folder,label FROM custom_folders WHERE owner=? AND lookup=?`,
		u.owner, strings.ToLower(label)).Scan(&folder.ID, &folder.Label)
	return folder, err
}

type imapList struct{ server.List }

func (cmd *imapList) Handle(conn server.Conn) error {
	user := conn.Context().User
	if user == nil {
		return server.ErrNotAuthenticated
	}
	if cmd.Mailbox == "" {
		mailboxes := make(chan *imap.MailboxInfo, 1)
		mailboxes <- &imap.MailboxInfo{Delimiter: "/", Attributes: []string{imap.NoSelectAttr}}
		close(mailboxes)
		return conn.WriteResp(&responses.List{Mailboxes: mailboxes, Subscribed: cmd.Subscribed})
	}
	boxes, err := user.ListMailboxes(cmd.Subscribed)
	if err != nil {
		return err
	}
	mailboxes := make(chan *imap.MailboxInfo, len(boxes))
	reference, pattern := strings.ToLower(cmd.Reference), strings.ToLower(cmd.Mailbox)
	for _, box := range boxes {
		if item, ok := box.(*imapMailbox); ok && item.subscriptionAncestor && !strings.Contains(pattern, "%") {
			continue
		}
		info, err := box.Info()
		if err != nil {
			return err
		}
		match := *info
		match.Name = strings.ToLower(info.Name)
		if match.Match(reference, pattern) {
			mailboxes <- info
		}
	}
	close(mailboxes)
	return conn.WriteResp(&responses.List{Mailboxes: mailboxes, Subscribed: cmd.Subscribed})
}

func (u imapUser) CreateMailbox(name string) error {
	// IMAP permits one trailing delimiter as a declaration of a parent path.
	_, err := u.app.saveCustomFolder(context.Background(), u.owner, "", strings.TrimSuffix(strings.TrimSpace(name), "/"))
	return imapFolderError(err)
}

func (u imapUser) DeleteMailbox(name string) error {
	box, err := u.GetMailbox(name)
	if err != nil {
		return err
	}
	folder := box.(*imapMailbox).folder
	if !customFolderID.MatchString(folder) {
		return errIMAPReadOnly
	}
	return imapFolderError(u.app.deleteCustomFolder(context.Background(), u.owner, folder))
}

func (u imapUser) RenameMailbox(oldName, newName string) error {
	box, err := u.GetMailbox(oldName)
	if err != nil {
		return err
	}
	folder := box.(*imapMailbox).folder
	if !customFolderID.MatchString(folder) {
		return errIMAPReadOnly
	}
	_, err = u.app.saveCustomFolder(context.Background(), u.owner, folder, newName)
	return imapFolderError(err)
}

func (m *imapMailbox) SetSubscribed(subscribed bool) error {
	if !customFolderID.MatchString(m.folder) {
		if subscribed {
			return nil
		}
		return errIMAPReadOnly
	}
	result, err := m.app.db.Exec(`UPDATE custom_folders SET subscribed=? WHERE owner=? AND folder=?`,
		subscribed, m.owner, m.folder)
	if err != nil {
		return imapFolderError(err)
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		err = sql.ErrNoRows
	}
	return imapFolderError(err)
}
