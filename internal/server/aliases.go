package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
)

func parseAliases(config string, accounts map[string][]byte) (map[string]string, error) {
	if strings.TrimSpace(config) == "" {
		return nil, nil
	}
	const format = "HARBOR_ALIASES must be a JSON object mapping lowercase aliases to configured primary addresses"
	decoder := json.NewDecoder(strings.NewReader(config))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New(format)
	}
	aliases := make(map[string]string)
	for decoder.More() {
		key, err := decoder.Token()
		alias, ok := key.(string)
		var owner string
		if err != nil || !ok || decoder.Decode(&owner) != nil {
			return nil, errors.New(format)
		}
		canonical, err := address(alias)
		if err != nil || canonical != alias || strings.ToLower(alias) != alias {
			return nil, errors.New("HARBOR_ALIASES contains an invalid or non-lowercase address")
		}
		if _, exists := accounts[alias]; exists {
			return nil, errors.New("HARBOR_ALIASES conflicts with a primary address")
		}
		if _, exists := aliases[alias]; exists {
			return nil, errors.New("HARBOR_ALIASES contains a duplicate alias")
		}
		if _, exists := accounts[owner]; !exists {
			return nil, errors.New("every HARBOR_ALIASES target must be a configured primary account")
		}
		aliases[alias] = owner
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return nil, errors.New(format)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New(format)
	}
	return aliases, nil
}

// A local alias keeps its envelope address while routing to its primary owner.
type mailDestination struct {
	address string
	owner   string
}

var errDestinationChanged = errors.New("recipient routing changed")

func (a *app) destination(ctx context.Context, db folderQueryer, to string) (mailDestination, error) {
	canonical := strings.ToLower(to)
	if _, exists := a.accounts[canonical]; exists {
		return mailDestination{address: canonical, owner: canonical}, nil
	}
	if owner := a.aliases[canonical]; owner != "" {
		return mailDestination{address: canonical, owner: owner}, nil
	}
	var owner string
	var primary, retired bool
	err := db.QueryRowContext(ctx, `SELECT email,TRUE,EXISTS(SELECT 1 FROM mailbox_retirements WHERE owner=email)
		FROM managed_accounts WHERE email=?
		UNION ALL SELECT a.owner,FALSE,EXISTS(SELECT 1 FROM mailbox_retirements r WHERE r.owner=a.owner)
		FROM managed_aliases a WHERE a.alias=?
		UNION ALL SELECT owner,TRUE,TRUE FROM mailbox_retirements WHERE owner=? LIMIT 1`,
		canonical, canonical, canonical).Scan(&owner, &primary, &retired)
	if errors.Is(err, sql.ErrNoRows) {
		return mailDestination{address: to}, nil
	}
	if err != nil {
		return mailDestination{}, err
	}
	if retired {
		return mailDestination{}, errors.Join(errDestinationChanged, errMailboxRetired)
	}
	if !primary {
		if _, err := a.baseAccountHash(ctx, db, owner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return mailDestination{}, errDestinationChanged
			}
			return mailDestination{}, err
		}
	}
	return mailDestination{address: canonical, owner: owner}, nil
}

func (a *app) checkDestination(ctx context.Context, db folderQueryer, address, owner string) error {
	target, err := a.destination(ctx, db, address)
	if err == nil && target.owner != owner {
		return errDestinationChanged
	}
	return err
}

func (a *app) managedAliases(ctx context.Context, owner string) (map[string]string, error) {
	query := `SELECT alias,owner FROM managed_aliases`
	var args []any
	if owner != "" {
		query += " WHERE owner=?"
		args = append(args, owner)
	}
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	aliases := make(map[string]string)
	for rows.Next() {
		var alias, owner string
		if err := rows.Scan(&alias, &owner); err != nil {
			return nil, err
		}
		aliases[alias] = owner
	}
	return aliases, rows.Err()
}

func (a *app) effectiveAliases(ctx context.Context, names map[string]bool) (map[string]string, error) {
	aliases, err := a.managedAliases(ctx, "")
	if err != nil {
		return nil, err
	}
	for alias, owner := range aliases {
		if names[alias] || !names[owner] {
			delete(aliases, alias)
		}
	}
	maps.Copy(aliases, a.aliases)
	return aliases, nil
}

func (a *app) addressesHTTP(w http.ResponseWriter, r *http.Request, owner string) {
	aliases := make([]string, 0)
	managed, err := a.managedAliases(r.Context(), owner)
	if letterError(w, err) {
		return
	}
	for alias := range managed {
		if a.aliases[alias] == "" {
			if _, err := a.baseAccountHash(r.Context(), a.db, alias); errors.Is(err, sql.ErrNoRows) {
				aliases = append(aliases, alias)
			} else if letterError(w, err) {
				return
			}
		}
	}
	for alias, target := range a.aliases {
		if target == owner {
			aliases = append(aliases, alias)
		}
	}
	slices.Sort(aliases)
	reply(w, http.StatusOK, struct {
		Primary string   `json:"primary"`
		Aliases []string `json:"aliases"`
	}{owner, aliases})
}
