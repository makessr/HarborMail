package main

import (
	"context"
	"database/sql"
	"errors"

	"github.com/go-sql-driver/mysql"
)

var errAccountSuspended = errors.New("account access is suspended")

// Credential issuers and account state changes share a write lock for this owner.
func (a *app) lockAccountAccess(ctx context.Context, tx *sql.Tx, owner string) (bool, error) {
	insert := `INSERT INTO account_access(owner,suspended) VALUES(?,FALSE)
		ON CONFLICT(owner) DO UPDATE SET suspended=account_access.suspended`
	if _, isMySQL := a.db.Driver().(*mysql.MySQLDriver); isMySQL {
		insert = `INSERT INTO account_access(owner,suspended) VALUES(?,FALSE)
			ON DUPLICATE KEY UPDATE suspended=suspended`
	}
	if _, err := tx.ExecContext(ctx, insert, owner); err != nil {
		return false, err
	}
	var suspended bool
	err := tx.QueryRowContext(ctx, `SELECT suspended FROM account_access WHERE owner=?`, owner).Scan(&suspended)
	return suspended, err
}

func (a *app) requireActiveAccount(ctx context.Context, tx *sql.Tx, owner string) error {
	suspended, err := a.lockAccountAccess(ctx, tx, owner)
	if err == nil && suspended {
		return errAccountSuspended
	}
	return err
}

func (a *app) suspendedAccounts(ctx context.Context) (map[string]bool, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT owner FROM account_access WHERE suspended=TRUE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	suspended := make(map[string]bool)
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, err
		}
		suspended[owner] = true
	}
	return suspended, rows.Err()
}
