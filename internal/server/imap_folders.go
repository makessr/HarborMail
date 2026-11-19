package server

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
	singleLevel := strings.Contains(pattern, "%")
	if strings.HasPrefix(pattern, "/") {
		reference = ""
		pattern = strings.TrimPrefix(pattern, "/")
	}
	if reference != "" && !strings.HasSuffix(reference, "/") {
		reference += "/"
	}
	for _, box := range boxes {
		if item, ok := box.(*imapMailbox); ok && item.subscriptionAncestor && !singleLevel {
			continue
		}
		info, err := box.Info()
		if err != nil {
			return err
		}
		name, inReference := strings.CutPrefix(strings.ToLower(info.Name), reference)
		if inReference && matchIMAPListPattern(name, pattern) {
			mailboxes <- info
		}
	}
	close(mailboxes)
	return conn.WriteResp(&responses.List{Mailboxes: mailboxes, Subscribed: cmd.Subscribed})
}

func matchIMAPListPattern(name, pattern string) bool {
	first := strings.IndexAny(pattern, "*%")
	if first < 0 {
		return name == pattern
	}
	name, matchesPrefix := strings.CutPrefix(name, pattern[:first])
	if !matchesPrefix {
		return false
	}
	pattern = pattern[first:]
	switch pattern {
	case "*":
		return true
	case "%":
		return !strings.Contains(name, "/")
	}

	// At each step, matched[n] says whether the processed pattern matches
	// name[:n]. Each wildcard can preserve or extend a matched prefix.
	matched := make([]bool, len(name)+1)
	matched[0] = true
	for i := range len(pattern) {
		switch char := pattern[i]; char {
		case '*', '%':
			for n := 1; n <= len(name); n++ {
				matched[n] = matched[n] || (matched[n-1] && (char == '*' || name[n-1] != '/'))
			}
		default:
			for n := len(name); n > 0; n-- {
				matched[n] = matched[n-1] && name[n-1] == char
			}
			matched[0] = false
		}
	}
	return matched[len(name)]
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
