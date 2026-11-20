package server

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"math"
	"slices"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/server"
)

func (m *imapMailbox) CopyMessages(uid bool, set *imap.SeqSet, name string) error {
	return m.transferMessages(uid, set, name, false)
}

func (m *imapMailbox) MoveMessages(uid bool, set *imap.SeqSet, name string) error {
	return m.transferMessages(uid, set, name, true)
}

func (m *imapMailbox) transferMessages(uid bool, set *imap.SeqSet, name string, move bool) error {
	box, err := m.imapUser.GetMailbox(name)
	if err != nil {
		return err
	}
	destination := box.(*imapMailbox).folder
	sameFolder := destination == m.folder
	if m.readOnly && (move || sameFolder) {
		return server.ErrMailboxReadOnly
	}
	indices := m.selectIndices(uid, set)
	if len(indices) == 0 {
		return nil
	}
	source := make([]imapMessage, 0, len(indices))
	for _, index := range indices {
		source = append(source, m.messages[index])
	}
	result, err := m.transferIMAPRows(source, destination, move, false)
	if isQuotaExceeded(err) {
		return errors.New("[OVERQUOTA] Mailbox storage quota exceeded")
	}
	if errors.Is(err, errIMAPMailboxChanged) {
		return err
	}
	if err != nil {
		log.Print("IMAP transfer: ", err)
		return errIMAPStorage
	}
	if sameFolder {
		return m.applyIMAPSnapshot(result.fresh)
	}
	if move {
		return m.removeFromIMAPSnapshot(result.removed)
	}
	return nil
}

func (m *imapMailbox) Expunge() error {
	if m.readOnly || m.folder == "trash" {
		return nil
	}
	result, err := m.transferIMAPRows(nil, "trash", true, true)
	if err != nil {
		log.Print("IMAP expunge: ", err)
		return errIMAPStorage
	}
	return m.removeFromIMAPSnapshot(result.removed)
}

func (m *imapMailbox) removeFromIMAPSnapshot(removed map[uint32]bool) error {
	if len(removed) == 0 {
		return nil
	}
	fresh := make([]imapMessage, 0, len(m.messages))
	for _, msg := range m.messages {
		if !removed[msg.Uid] {
			fresh = append(fresh, msg)
		}
	}
	return m.applyIMAPSnapshot(fresh)
}

func (m *imapMailbox) reportCount(n uint32, name string) error {
	if m.writeResponse == nil {
		return nil
	}
	return m.writeResponse(imap.NewUntaggedResp([]interface{}{n, imap.RawString(name)}))
}

type imapTransferResult struct {
	fresh   []imapMessage
	removed map[uint32]bool
}

func (m *imapMailbox) transferIMAPRows(source []imapMessage, destination string, move, markedOnly bool) (imapTransferResult, error) {
	var result imapTransferResult
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := m.app.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return result, err
	}
	if err := verifyFolder(ctx, tx, m.owner, destination); err != nil {
		return result, err
	}
	if markedOnly {
		messages, err := queryIMAPMessages(ctx, tx, m.owner, m.folder)
		if err != nil {
			return result, err
		}
		for _, msg := range messages {
			if slices.Contains(msg.Flags, imap.DeletedFlag) {
				source = append(source, msg)
			}
		}
		if len(source) == 0 {
			return result, nil
		}
	}
	var next uint32
	if err := tx.QueryRowContext(ctx, `SELECT next_uid FROM imap_uid_state WHERE id=1`).Scan(&next); err != nil {
		return result, err
	}
	if err := assignMailboxUIDs(ctx, tx, m.owner, destination, &next); err != nil {
		return result, err
	}
	if uint64(next)+uint64(len(source)) > math.MaxUint32 {
		return result, errors.New("IMAP UID space exhausted")
	}
	if move {
		result.removed = make(map[uint32]bool, len(source))
	}
	for _, msg := range source {
		id, err := m.transferIMAPRow(ctx, tx, msg, destination, move)
		if err != nil {
			return result, err
		}
		if move {
			_, err = tx.ExecContext(ctx, `UPDATE imap_uids SET uid=?,folder=? WHERE letter_id=?`, next, destination, id)
			if err == nil {
				_, err = tx.ExecContext(ctx, `DELETE FROM imap_deleted WHERE letter_id=?`, id)
			}
			if err == nil {
				_, err = tx.ExecContext(ctx, `UPDATE web_drafts SET revision=revision+1 WHERE letter_id=?`, id)
			}
			result.removed[msg.Uid] = true
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO imap_uids (uid,folder,letter_id) VALUES(?,?,?)`, next, destination, id)
			if err == nil && destination != "trash" {
				_, err = tx.ExecContext(ctx, `INSERT INTO imap_deleted (letter_id,uid)
					SELECT ?,? FROM imap_deleted WHERE letter_id=? AND uid=?`,
					id, next, msg.id, msg.Uid)
			}
			if err == nil && destination == "drafts" {
				_, err = tx.ExecContext(ctx, `INSERT INTO web_drafts (letter_id,revision)
					SELECT ?,1 FROM web_drafts WHERE letter_id=?`, id, msg.id)
			}
		}
		if err != nil {
			return result, err
		}
		next++
	}
	if _, err := tx.ExecContext(ctx, `UPDATE imap_uid_state SET next_uid=? WHERE id=1`, next); err != nil {
		return result, err
	}
	if destination == m.folder {
		result.fresh, err = queryIMAPMessages(ctx, tx, m.owner, m.folder)
		if err != nil {
			return result, err
		}
	}
	if err := m.app.commitMailbox(tx, m.owner); err != nil {
		return result, err
	}
	return result, nil
}

func (m *imapMailbox) transferIMAPRow(ctx context.Context, tx *sql.Tx, msg imapMessage, destination string, move bool) (int64, error) {
	// Read/flag state and MIME bytes are copied from the current row, never the cached session snapshot.
	origin := `CASE WHEN ? IN ('inbox','sent') THEN ? ELSE origin END`
	previous := `CASE WHEN ?='trash' AND folder<>'trash' THEN folder WHEN ?='trash' THEN previous ELSE '' END`
	guard := ` WHERE owner=? AND id=? AND folder=? AND EXISTS
		(SELECT 1 FROM imap_uids WHERE letter_id=? AND folder=? AND uid=?)`
	args := []any{destination, destination, destination, destination, destination,
		m.owner, msg.id, m.folder, msg.id, m.folder, msg.Uid}
	var statement string
	if move {
		statement = `UPDATE letters SET previous=` + previous + `,origin=` + origin + `,folder=?` + guard
	} else {
		statement = `INSERT INTO letters (owner,sender,recipient,subject,body,stamp,seen,star,raw,detail_meta,previous,origin,folder)
			SELECT owner,sender,recipient,subject,body,stamp,seen,star,raw,detail_meta,` + previous + `,` + origin + `,?
			FROM letters` + guard
	}
	result, err := tx.ExecContext(ctx, statement, args...)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if count != 1 {
		return 0, errIMAPMailboxChanged
	}
	if move {
		return msg.id, nil
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO web_letter_recipients(letter_id,to_list,cc_list,bcc_list)
		SELECT ?,to_list,cc_list,bcc_list FROM web_letter_recipients WHERE letter_id=?`, id, msg.id); err != nil {
		return 0, err
	}
	return id, copyThread(ctx, tx, m.owner, msg.id, id)
}
