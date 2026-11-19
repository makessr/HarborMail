package server

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

//go:embed admin.html
var adminTemplate string

func parseAdmins(setting string, accounts map[string][]byte) (map[string]bool, error) {
	admins := make(map[string]bool)
	if strings.TrimSpace(setting) == "" {
		return admins, nil
	}
	for _, value := range strings.Split(setting, ",") {
		owner := strings.ToLower(strings.TrimSpace(value))
		if _, exists := accounts[owner]; !exists || owner == "" {
			return nil, errors.New("HARBOR_ADMINS must list configured primary mailbox accounts")
		}
		admins[owner] = true
	}
	return admins, nil
}

func (a *app) replySession(w http.ResponseWriter, owner string) {
	role := "user"
	if a.admins[owner] {
		role = "admin"
	} else if len(a.domainAdmins[owner]) > 0 {
		role = "domain_admin"
	}
	reply(w, http.StatusOK, struct {
		Email        string `json:"email"`
		Role         string `json:"role"`
		RecoveryMode bool   `json:"recoveryMode,omitempty"`
	}{owner, role, a.recoveryMode})
}

func (a *app) adminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	session, err := a.browserSession(r)
	if err != nil {
		http.Error(w, "无法读取登录会话", http.StatusInternalServerError)
		return
	}
	if session.owner == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	scoped := !a.admins[session.owner]
	if scoped {
		if len(a.domainAdmins[session.owner]) == 0 {
			http.Redirect(w, r, "/?notice=admin-forbidden", http.StatusSeeOther)
			return
		}
		switch r.URL.Path {
		case "/admin", "/admin/":
			http.Redirect(w, r, "/admin/accounts", http.StatusSeeOther)
			return
		case "/admin/accounts", "/admin/domains", "/admin/aliases":
		default:
			http.Redirect(w, r, "/admin/accounts", http.StatusSeeOther)
			return
		}
	}
	a.touchBrowserSession(r, session)
	page := []byte(adminTemplate)
	if scoped {
		page = bytes.Replace(page, []byte(`<body class="admin">`), []byte(`<body class="admin" data-domain-admin>`), 1)
	}
	if a.recoveryMode {
		page = bytes.Replace(page, []byte(`<span id="admin-recovery-mode" hidden>`), []byte(`<span id="admin-recovery-mode">`), 1)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

func (a *app) adminOnly(handler authorized) http.HandlerFunc {
	return a.auth(func(w http.ResponseWriter, r *http.Request, owner string) {
		if !a.admins[owner] {
			fail(w, http.StatusForbidden, "当前账号没有管理权限")
			return
		}
		handler(w, r, owner)
	})
}

type adminAccount struct {
	Email     string   `json:"email"`
	Admin     bool     `json:"admin"`
	Aliases   []string `json:"aliases"`
	Suspended bool     `json:"suspended"`
	Managed   bool     `json:"managed"`
	Retired   bool     `json:"retired"`
}

func (a *app) adminAccounts(names, suspended, retired map[string]bool, effectiveAliases map[string]string) []adminAccount {
	aliases := make(map[string][]string)
	for alias, target := range effectiveAliases {
		aliases[target] = append(aliases[target], alias)
	}
	accounts := make([]adminAccount, 0, len(names))
	for email := range names {
		addresses := aliases[email]
		slices.Sort(addresses)
		_, configured := a.accounts[email]
		accounts = append(accounts, adminAccount{
			Email: email, Admin: a.admins[email], Aliases: addresses, Suspended: suspended[email], Managed: !configured, Retired: retired[email],
		})
	}
	slices.SortFunc(accounts, func(left, right adminAccount) int { return strings.Compare(left.Email, right.Email) })
	return accounts
}

type adminOverviewSnapshot struct {
	Accounts    []adminAccount     `json:"accounts"`
	Queue       map[string]int64   `json:"queue"`
	Domains     []adminDomain      `json:"domains"`
	Trend       []adminActivityDay `json:"trend"`
	Recent      []adminQueueItem   `json:"recent"`
	GeneratedAt int64              `json:"generatedAt"`
	OldestDue   int64              `json:"oldestDue"`
}

func (a *app) replyOverview(w http.ResponseWriter, owner string, snapshot adminOverviewSnapshot) {
	reply(w, http.StatusOK, struct {
		adminOverviewSnapshot
		Email           string `json:"email"`
		Domain          string `json:"domain"`
		Database        string `json:"database"`
		RelayConfigured bool   `json:"relayConfigured"`
		SpamScanning    bool   `json:"spamScanning"`
	}{snapshot, owner, a.domain, a.dbDriver, a.relay.Address != "" || len(a.domainRelays) > 0, a.spamScanner != nil})
}

func (a *app) adminOverview(w http.ResponseWriter, r *http.Request, owner string) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	var cached adminOverviewSnapshot
	hit, reachable := a.adminCache.read(ctx, "overview", &cached)
	if hit && cached.GeneratedAt > 0 && cached.Queue != nil {
		for i := range cached.Recent {
			cached.Recent[i].CanRetry = a.canRetryDelivery(cached.Recent[i], time.Now().Unix())
		}
		a.replyOverview(w, owner, cached)
		return
	}
	rows, err := a.db.QueryContext(ctx, `SELECT state,COUNT(*) FROM outbox GROUP BY state`)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "无法读取投递队列，请稍后刷新")
		return
	}
	defer rows.Close()
	queue := make(map[string]int64)
	for rows.Next() {
		var state string
		var count int64
		if err := rows.Scan(&state, &count); err != nil {
			fail(w, http.StatusServiceUnavailable, "无法读取投递队列，请稍后刷新")
			return
		}
		queue[state] = count
	}
	if rows.Err() != nil {
		fail(w, http.StatusServiceUnavailable, "无法读取投递队列，请稍后刷新")
		return
	}
	rows.Close()
	suspended, err := a.suspendedAccounts(ctx)
	if letterError(w, err) {
		return
	}
	retired, err := a.retiredAccounts(ctx)
	if letterError(w, err) {
		return
	}
	names, err := a.allAccountNames(ctx)
	if letterError(w, err) {
		return
	}
	aliases, err := a.effectiveAliases(ctx, names)
	if letterError(w, err) {
		return
	}
	domains, err := a.adminDomains(ctx, names, aliases)
	if letterError(w, err) {
		return
	}
	now := time.Now()
	trend, err := a.adminActivity(ctx, now)
	if letterError(w, err) {
		return
	}
	recent, err := a.readAdminQueue(ctx, "", nil, 6)
	if letterError(w, err) {
		return
	}
	var oldestDue int64
	if err := a.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(due),0) FROM outbox
		WHERE state IN ('queued','scheduled','undo') AND due>0 AND due<=?`, now.Unix()).Scan(&oldestDue); letterError(w, err) {
		return
	}
	snapshot := adminOverviewSnapshot{
		Accounts: a.adminAccounts(names, suspended, retired, aliases), Queue: queue, Domains: domains,
		Trend: trend, Recent: recent, GeneratedAt: now.Unix(), OldestDue: oldestDue,
	}
	if reachable {
		a.adminCache.write(ctx, "overview", snapshot, overviewCacheTTL)
	}
	a.replyOverview(w, owner, snapshot)
}

type adminActivityDay struct {
	Day     int64 `json:"day"`
	Account int64 `json:"account"`
	Aliases int64 `json:"aliases"`
	Retry   int64 `json:"retry"`
}

func (a *app) adminActivity(ctx context.Context, now time.Time) ([]adminActivityDay, error) {
	const day = int64(24 * time.Hour / time.Second)
	start := now.UTC().Truncate(24*time.Hour).Unix() - 6*day
	result := make([]adminActivityDay, 7)
	for index := range result {
		result[index].Day = start + int64(index)*day
	}
	rows, err := a.db.QueryContext(ctx, `SELECT stamp-(stamp%86400),1,COUNT(*) FROM admin_account_actions
		WHERE stamp>=? AND stamp<? GROUP BY stamp-(stamp%86400)
		UNION ALL SELECT stamp-(stamp%86400),2,COUNT(*) FROM admin_alias_actions
		WHERE stamp>=? AND stamp<? GROUP BY stamp-(stamp%86400)
		UNION ALL SELECT stamp-(stamp%86400),3,COUNT(*) FROM admin_delivery_retries
		WHERE stamp>=? AND stamp<? GROUP BY stamp-(stamp%86400)`,
		start, start+7*day, start, start+7*day, start, start+7*day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var bucket, count int64
		var kind int
		if err := rows.Scan(&bucket, &kind, &count); err != nil {
			return nil, err
		}
		item := &result[(bucket-start)/day]
		switch kind {
		case 1:
			item.Account = count
		case 2:
			item.Aliases = count
		case 3:
			item.Retry = count
		}
	}
	return result, rows.Err()
}
