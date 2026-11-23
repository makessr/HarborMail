// Package mailstore contains the SQL shared by Harbor Mail's protocol adapters.
package mailstore

import (
	"context"
	"database/sql"
)

type LetterRow struct {
	Owner, Sender, Recipient string
	Subject, Body            string
	Stamp                    int64
	Folder, Origin           string
	Seen, Star               bool
	Raw, Detail              []byte
	POPOctets                int64
}

func InsertLetter(ctx context.Context, tx *sql.Tx, row LetterRow) (int64, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO letters
		(owner,sender,recipient,subject,body,stamp,folder,origin,seen,star,raw,detail_meta,pop_octets)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, row.Owner, row.Sender, row.Recipient, row.Subject, row.Body,
		row.Stamp, row.Folder, row.Origin, row.Seen, row.Star, row.Raw, row.Detail, row.POPOctets)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}
