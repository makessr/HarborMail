package server

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"
)

const (
	threadLimit   = 100
	threadDepth   = 16
	threadFolders = `l.folder NOT IN ('drafts','junk','trash')`
	threadColumns = `l.id,l.sender,l.recipient,l.subject,l.stamp,l.folder,substr(l.body,1,200),t.message_key,t.root_key,t.parent_key`
)

func threadKey(id string) []byte {
	if id == "" {
		return nil
	}
	hash := sha256.Sum256([]byte(id))
	return hash[:]
}

func indexThread(tx *sql.Tx, owner string, id int64, raw []byte) error {
	header := identifyMessage(raw)
	var root, parent string
	if len(header.references) > 0 {
		root = header.references[0]
		parent = header.references[len(header.references)-1]
	}
	if len(header.parents) > 0 {
		parent = header.parents[0]
	}
	_, err := tx.Exec(`INSERT INTO mail_threads (letter_id,owner,message_key,root_key,parent_key) VALUES(?,?,?,?,?)`,
		id, owner, threadKey(header.id), threadKey(root), threadKey(parent))
	return err
}

func copyThread(ctx context.Context, tx *sql.Tx, owner string, source, copyID int64) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO mail_threads (letter_id,owner,message_key,root_key,parent_key)
		SELECT ?,owner,message_key,root_key,parent_key FROM mail_threads WHERE owner=? AND letter_id=?`, copyID, owner, source)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 0 {
		return err
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT substr(raw,1,?) FROM letters WHERE owner=? AND id=?`,
		threadHeaderLimit, owner, copyID).Scan(&raw); err != nil {
		return err
	}
	return indexThread(tx, owner, copyID, raw)
}

func (a *app) backfillThreads(ctx context.Context) error {
	var cursor int64
	owners := make(map[string]struct{})
	for {
		rows, err := a.db.QueryContext(ctx, `SELECT l.id FROM letters l LEFT JOIN mail_threads t ON t.letter_id=l.id
			WHERE l.id>? AND t.letter_id IS NULL ORDER BY l.id LIMIT 64`, cursor)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			owner, err := a.indexMissingThread(ctx, id)
			if err != nil {
				return err
			}
			if owner != "" {
				owners[owner] = struct{}{}
			}
			cursor = id
		}
	}
	for owner := range owners {
		a.events.notify(owner)
	}
	return nil
}

func (a *app) indexMissingThread(ctx context.Context, id int64) (string, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return "", err
	}
	var owner string
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT l.owner,substr(l.raw,1,?) FROM letters l LEFT JOIN mail_threads t ON t.letter_id=l.id
		WHERE l.id=? AND t.letter_id IS NULL`, threadHeaderLimit, id).Scan(&owner, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := indexThread(tx, owner, id, raw); err != nil {
		return "", err
	}
	return owner, tx.Commit()
}

type threadEntry struct {
	ID      int64  `json:"id"`
	From    string `json:"from"`
	To      string `json:"to"`
	Subject string `json:"subject"`
	Date    int64  `json:"date"`
	Folder  string `json:"folder"`
	Preview string `json:"preview,omitempty"`
	keys    [3][]byte
}

type threadScanner interface{ Scan(...any) error }

func scanThreadEntry(row threadScanner) (threadEntry, error) {
	var item threadEntry
	err := row.Scan(&item.ID, &item.From, &item.To, &item.Subject, &item.Date, &item.Folder, &item.Preview,
		&item.keys[0], &item.keys[1], &item.keys[2])
	return item, err
}

func placeholders(n int) string {
	return strings.Repeat("?,", n-1) + "?"
}

func (a *app) conversation(ctx context.Context, owner string, id int64) ([]threadEntry, error) {
	seed, err := scanThreadEntry(a.db.QueryRowContext(ctx, `SELECT `+threadColumns+`
		FROM letters l LEFT JOIN mail_threads t ON t.letter_id=l.id AND t.owner=l.owner
		WHERE l.owner=? AND l.id=? AND `+threadFolders, owner, id))
	if err != nil {
		return nil, err
	}
	items := []threadEntry{seed}
	seen := make(map[string]bool)
	for start, depth := 0, 0; start < len(items) && len(items) < threadLimit && depth < threadDepth; depth++ {
		var keys []any
		for _, item := range items[start:] {
			for _, key := range item.keys {
				if len(key) != 0 && !seen[string(key)] {
					seen[string(key)] = true
					keys = append(keys, key)
				}
			}
		}
		if len(keys) == 0 {
			break
		}
		start = len(items)
		args := []any{owner}
		for range 3 {
			args = append(args, keys...)
		}
		for _, item := range items {
			args = append(args, item.ID)
		}
		args = append(args, threadLimit-len(items))
		in := placeholders(len(keys))
		rows, err := a.db.QueryContext(ctx, `SELECT `+threadColumns+`
			FROM mail_threads t JOIN letters l ON l.id=t.letter_id AND l.owner=t.owner WHERE t.owner=? AND `+threadFolders+`
			AND (t.message_key IN (`+in+`) OR t.root_key IN (`+in+`) OR t.parent_key IN (`+in+`))
			AND l.id NOT IN (`+placeholders(len(items))+`) ORDER BY l.id DESC LIMIT ?`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			item, err := scanThreadEntry(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			items = append(items, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	slices.SortFunc(items, func(a, b threadEntry) int {
		if date := cmp.Compare(a.Date, b.Date); date != 0 {
			return date
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return items, nil
}

func (a *app) getThread(w http.ResponseWriter, r *http.Request, owner string) {
	items, err := a.conversation(r.Context(), owner, letterID(r))
	if !letterError(w, err) {
		reply(w, 200, items)
	}
}
