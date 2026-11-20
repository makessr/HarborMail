package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
)

var errSendAs = errors.New("sender identity is no longer authorized")

func parseSendAliases(setting string) (map[string]bool, error) {
	result := make(map[string]bool)
	if strings.TrimSpace(setting) == "" {
		return result, nil
	}
	for item := range strings.SplitSeq(setting, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		email, err := address(item)
		if err != nil || email != item {
			return nil, errors.New("HARBOR_SEND_AS_ALIASES must contain comma-separated alias email addresses")
		}
		result[email] = true
	}
	return result, nil
}

func (m letter) wireFrom() string {
	if m.author != "" {
		return m.author
	}
	return m.From
}

func (a *app) outboxAuthorSQL() string {
	lookup := `json_extract(CASE WHEN json_valid(e.envelope) THEN e.envelope ELSE '{}' END,'$.Author')`
	if a.dbDriver == "mysql" {
		// Mailbox identities are ASCII. Match the outbox sender column's collation.
		lookup = "CONVERT(JSON_UNQUOTE(" + lookup + ") USING ascii) COLLATE ascii_bin"
	}
	return `COALESCE(NULLIF((SELECT ` + lookup + ` FROM web_outbox_envelopes e
		WHERE e.outbox_id=outbox.id),''),sender)`
}

func (a *app) requireSenderIdentity(ctx context.Context, db folderQueryer, owner, author string) error {
	if author == "" || author == owner {
		return nil
	}
	if !a.sendAliases[author] {
		return errSendAs
	}
	err := a.checkDestination(ctx, db, author, owner)
	if errors.Is(err, errDestinationChanged) {
		return errSendAs
	}
	return err
}

func (a *app) validateSenderIdentity(w http.ResponseWriter, ctx context.Context, db folderQueryer, owner, author string) bool {
	err := a.requireSenderIdentity(ctx, db, owner, author)
	if errors.Is(err, errSendAs) {
		fail(w, http.StatusUnprocessableEntity, "当前邮箱无权使用此发件地址，请选择主邮箱或已授权别名")
		return false
	}
	return !letterError(w, err)
}

func (a *app) senderIdentities(w http.ResponseWriter, r *http.Request, owner string) {
	aliases := make([]string, 0)
	var managed map[string]string
	if len(a.sendAliases) != 0 {
		var err error
		managed, err = a.managedAliases(r.Context(), owner)
		if letterError(w, err) {
			return
		}
	}
	for alias := range a.sendAliases {
		if alias == owner || (a.aliases[alias] != owner && managed[alias] != owner) {
			continue
		}
		err := a.requireSenderIdentity(r.Context(), a.db, owner, alias)
		if errors.Is(err, errSendAs) {
			continue
		}
		if letterError(w, err) {
			return
		}
		aliases = append(aliases, alias)
	}
	slices.Sort(aliases)
	w.Header().Set("Cache-Control", "no-store")
	reply(w, http.StatusOK, map[string]any{"primary": owner, "aliases": aliases})
}
