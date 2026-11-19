package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/emersion/go-imap"
)

var errIMAPMailboxChanged = errors.New("Mailbox membership changed; refresh the selected folder")

func mailboxFlags(seen, starred, deleted, draft bool) []string {
	var flags []string
	if seen {
		flags = append(flags, imap.SeenFlag)
	}
	if starred {
		flags = append(flags, imap.FlaggedFlag)
	}
	if deleted {
		flags = append(flags, imap.DeletedFlag)
	}
	if draft {
		flags = append(flags, imap.DraftFlag)
	}
	return flags
}

func readsMessage(msg *imap.Message) bool {
	for section, body := range msg.Body {
		if !section.Peek && body != nil {
			return true
		}
	}
	return false
}

func (m *imapMailbox) UpdateMessagesFlags(uid bool, set *imap.SeqSet, op imap.FlagsOp, flags []string) error {
	if m.readOnly {
		return errIMAPReadOnly
	}
	var seen, starred, deleted *bool
	if op == imap.SetFlags {
		no := false
		seen, starred, deleted = &no, &no, &no
	}
	value := op != imap.RemoveFlags
	for _, flag := range flags {
		switch imap.CanonicalFlag(flag) {
		case imap.SeenFlag:
			seen = &value
		case imap.FlaggedFlag:
			starred = &value
		case imap.DeletedFlag:
			if m.folder == "trash" && value {
				return errors.New("Trash does not allow permanent deletion")
			}
			deleted = &value
		case imap.DraftFlag:
			if m.folder != "drafts" {
				return errors.New("Draft flags belong to the Drafts mailbox")
			}
		default:
			return errors.New("Unsupported IMAP flag")
		}
	}
	if seen == nil && starred == nil && deleted == nil {
		return nil
	}
	return m.persistFlags(m.selectIndices(uid, set), seen, starred, deleted)
}

func (m *imapMailbox) selectIndices(uid bool, set *imap.SeqSet) []int {
	maximum := uint32(len(m.messages))
	if uid {
		maximum = m.maxUID()
	}
	set = imapSequence(set, maximum)
	var indices []int
	for i, msg := range m.messages {
		number := uint32(i + 1)
		if uid {
			number = msg.Uid
		}
		if set.Contains(number) {
			indices = append(indices, i)
		}
	}
	return indices
}

func (m *imapMailbox) persistFlags(indices []int, seen, starred, deleted *bool) error {
	if len(indices) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := m.app.db.BeginTx(ctx, nil)
	if err != nil {
		log.Print("IMAP flag transaction: ", err)
		return errIMAPStorage
	}
	defer tx.Rollback()
	if deleted != nil {
		if err := lockIMAPUIDs(ctx, tx); err != nil {
			log.Print("IMAP lock deletion flags: ", err)
			return errIMAPStorage
		}
	}
	flags := make([][]string, len(indices))
	for j, i := range indices {
		msg := m.messages[i]
		result, err := tx.ExecContext(ctx, `UPDATE letters SET seen=coalesce(?,seen), star=coalesce(?,star)
			WHERE owner=? AND id=? AND folder=? AND EXISTS
			(SELECT 1 FROM imap_uids WHERE letter_id=? AND folder=? AND uid=?)`,
			seen, starred, m.owner, msg.id, m.folder, msg.id, m.folder, msg.Uid)
		if err != nil {
			log.Print("IMAP update flags: ", err)
			return errIMAPStorage
		}
		count, err := result.RowsAffected()
		if err != nil {
			log.Print("IMAP affected flags: ", err)
			return errIMAPStorage
		}
		if count != 1 {
			return errIMAPMailboxChanged
		}
		if deleted != nil {
			var err error
			if *deleted {
				var marked bool
				err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM imap_deleted WHERE letter_id=?)`, msg.id).Scan(&marked)
				if err == nil && !marked {
					_, err = tx.ExecContext(ctx, `INSERT INTO imap_deleted (letter_id,uid) VALUES (?,?)`, msg.id, msg.Uid)
				}
			} else {
				_, err = tx.ExecContext(ctx, `DELETE FROM imap_deleted WHERE letter_id=? AND uid=?`, msg.id, msg.Uid)
			}
			if err != nil {
				log.Print("IMAP store deletion mark: ", err)
				return errIMAPStorage
			}
		}
		var isSeen, isStarred, isDeleted bool
		if err := tx.QueryRowContext(ctx, `SELECT seen,star,
			CASE WHEN EXISTS (SELECT 1 FROM imap_deleted WHERE letter_id=? AND uid=?) THEN 1 ELSE 0 END
			FROM letters WHERE owner=? AND id=?`, msg.id, msg.Uid, m.owner, msg.id).
			Scan(&isSeen, &isStarred, &isDeleted); err != nil {
			log.Print("IMAP read flags: ", err)
			return errIMAPStorage
		}
		flags[j] = mailboxFlags(isSeen, isStarred, isDeleted, m.folder == "drafts")
	}
	if err := m.app.commitMailbox(tx, m.owner); err != nil {
		log.Print("IMAP commit flags: ", err)
		return errIMAPStorage
	}
	for j, i := range indices {
		m.messages[i].Flags = flags[j]
	}
	return nil
}
