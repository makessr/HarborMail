package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

type adminAlias struct {
	Alias   string `json:"alias"`
	Owner   string `json:"owner"`
	Managed bool   `json:"managed"`
	SendAs  bool   `json:"sendAs"`
	State   string `json:"state"`
}

func (a *app) adminAliasList(w http.ResponseWriter, r *http.Request, actor string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	managed, err := a.managedAliases(ctx, "")
	if letterError(w, err) {
		return
	}
	names, err := a.allAccountNames(ctx)
	if letterError(w, err) {
		return
	}
	admins, err := a.adminRoleSet(ctx, a.db)
	if letterError(w, err) {
		return
	}
	items := make([]adminAlias, 0, len(managed)+len(a.aliases))
	for alias, owner := range a.aliases {
		if a.canManageDomainAlias(actor, alias, owner, admins) {
			items = append(items, adminAlias{Alias: alias, Owner: owner, State: "active"})
		}
	}
	for alias, owner := range managed {
		if !a.canManageDomainAlias(actor, alias, owner, admins) {
			continue
		}
		state := "active"
		primary := names[alias]
		valid := names[owner]
		switch {
		case primary || a.aliases[alias] != "":
			state = "shadowed"
		case !valid:
			state = "orphaned"
		}
		items = append(items, adminAlias{Alias: alias, Owner: owner, Managed: true, State: state})
	}
	for i := range items {
		if items[i].State != "active" {
			continue
		}
		items[i].SendAs, err = a.senderAliasAllowed(ctx, a.db, items[i].Owner, items[i].Alias)
		if letterError(w, err) {
			return
		}
	}
	slices.SortFunc(items, func(left, right adminAlias) int { return strings.Compare(left.Alias, right.Alias) })
	owners := make([]string, 0, len(names))
	for owner := range names {
		if admins[actor] || (!admins[owner] && a.domainAdmins[actor][emailDomain(owner)]) {
			owners = append(owners, owner)
		}
	}
	slices.Sort(owners)
	reply(w, 200, map[string]any{"email": actor, "aliases": items, "owners": owners})
}

func (a *app) adminAliasChange(w http.ResponseWriter, r *http.Request, actor string) {
	var input struct{ Action, Alias, Owner, Password string }
	if !decode(w, r, &input) {
		return
	}
	alias := strings.ToLower(strings.TrimSpace(input.Alias))
	owner := strings.ToLower(strings.TrimSpace(input.Owner))
	canonical, err := address(alias)
	if err != nil || canonical != alias || (input.Action != "alias_add" && input.Action != "alias_remove" && input.Action != "alias_send_grant" && input.Action != "alias_send_revoke") {
		fail(w, 400, "请输入有效的别名地址")
		return
	}
	if _, err := a.authenticate(actor, input.Password); err != nil {
		if errors.Is(err, errLoginBusy) {
			fail(w, 429, "验证请求过多，请稍后重试")
		} else {
			fail(w, 403, "管理员主邮箱密码不正确")
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if letterError(w, err) {
		return
	}
	defer tx.Rollback()
	// Use the same ordering as local delivery and mailbox mutations.
	if letterError(w, lockIMAPUIDs(ctx, tx)) ||
		browserSessionError(w, a.lockBrowserSession(ctx, tx, actor, browserSessionDigest(r))) {
		return
	}
	actorAdmin, err := a.isGlobalAdmin(ctx, tx, actor)
	if letterError(w, err) {
		return
	}
	if !actorAdmin {
		targetAdmin, err := a.isGlobalAdmin(ctx, tx, owner)
		if letterError(w, err) {
			return
		}
		if targetAdmin || !a.domainAdmins[actor][emailDomain(alias)] || !a.domainAdmins[actor][emailDomain(owner)] {
			fail(w, http.StatusForbidden, "当前账号无权管理此别名或目标邮箱")
			return
		}
	}
	var previous string
	err = tx.QueryRowContext(ctx, `SELECT owner FROM managed_aliases WHERE alias=?`, alias).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		letterError(w, err)
		return
	}
	if input.Action == "alias_add" {
		if err == nil {
			fail(w, 409, "此别名已存在")
			return
		}
		if _, err := a.baseAccountHash(ctx, tx, owner); errors.Is(err, sql.ErrNoRows) {
			fail(w, 400, "请选择已配置的主邮箱")
			return
		} else if letterError(w, err) {
			return
		}
		if _, err := a.baseAccountHash(ctx, tx, alias); err == nil || a.aliases[alias] != "" {
			fail(w, 409, "此地址已配置为主邮箱或收信别名")
			return
		} else if !errors.Is(err, sql.ErrNoRows) && letterError(w, err) {
			return
		}
		var reserved bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_access WHERE owner=?
			UNION ALL SELECT 1 FROM mailbox_retirements WHERE owner=?)`, alias, owner).Scan(&reserved)
		if letterError(w, err) {
			return
		}
		if reserved {
			fail(w, http.StatusConflict, "别名地址已保留，或目标邮箱已回收")
			return
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO managed_aliases(alias,owner) VALUES(?,?)`, alias, owner)
	} else if input.Action == "alias_remove" {
		if err != nil || previous != owner {
			fail(w, 409, "此别名已变化或由服务器配置管理，请刷新后重试")
			return
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM managed_aliases WHERE alias=? AND owner=?`, alias, owner)
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM alias_send_permissions WHERE alias=? AND owner=?`, alias, owner)
		}
	} else {
		target, lookupErr := a.destination(ctx, tx, alias)
		if lookupErr != nil && !errors.Is(lookupErr, errDestinationChanged) {
			letterError(w, lookupErr)
			return
		}
		if owner == alias || target.owner == "" || target.owner != owner {
			fail(w, 409, "该别名当前未绑定此主邮箱")
			return
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM alias_send_permissions WHERE alias=? AND owner=?`, alias, owner); letterError(w, err) {
			return
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO alias_send_permissions(alias,owner,allowed) VALUES(?,?,?)`, alias, owner, input.Action == "alias_send_grant")
	}
	if letterError(w, err) {
		return
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_alias_actions(actor,target,alias,action,stamp)
		VALUES(?,?,?,?,?)`, actor, owner, alias, input.Action, time.Now().Unix()); letterError(w, err) {
		return
	}
	if !letterError(w, a.commitMailbox(tx, owner)) {
		w.WriteHeader(http.StatusNoContent)
	}
}
