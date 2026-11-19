package main

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

type adminDomain struct {
	Name     string `json:"name"`
	Accounts int    `json:"accounts"`
	Aliases  int    `json:"aliases"`
	Managed  bool   `json:"managed"`
}

func emailDomain(email string) string {
	_, domain, _ := strings.Cut(email, "@")
	return strings.TrimSuffix(strings.ToLower(domain), ".")
}

func (a *app) adminDomains(ctx context.Context, names map[string]bool, aliases map[string]string) ([]adminDomain, error) {
	counts := make(map[string]adminDomain)
	rows, err := a.db.QueryContext(ctx, `SELECT name FROM managed_domains`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		counts[name] = adminDomain{Name: name, Managed: true}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for address := range names {
		name := emailDomain(address)
		domain := counts[name]
		domain.Name = name
		domain.Accounts++
		counts[name] = domain
	}
	for address := range aliases {
		name := emailDomain(address)
		domain := counts[name]
		domain.Name = name
		domain.Aliases++
		counts[name] = domain
	}
	domains := make([]adminDomain, 0, len(counts))
	for _, domain := range counts {
		domains = append(domains, domain)
	}
	slices.SortFunc(domains, func(left, right adminDomain) int { return strings.Compare(left.Name, right.Name) })
	return domains, nil
}

func validDNSName(name string) bool {
	if len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !acmeDNSLabel.MatchString(label) {
			return false
		}
	}
	return true
}

type dnsCheck struct {
	State string `json:"state"`
	Count int    `json:"count,omitempty"`
}

type domainDNS struct {
	MX    dnsCheck  `json:"mx"`
	Hosts []*net.MX `json:"hosts,omitempty"`
	SPF   dnsCheck  `json:"spf"`
	DMARC dnsCheck  `json:"dmarc"`
	DKIM  dnsCheck  `json:"dkim"`
}

var dnsCheckSlots = make(chan struct{}, 4)

func (a *app) adminChangeDomain(w http.ResponseWriter, r *http.Request, actor string) {
	var input struct{ Domain, Action, Password string }
	if !decode(w, r, &input) {
		return
	}
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(input.Domain)), ".")
	if !strings.Contains(domain, ".") || len("_dmarc."+domain) > 253 || !validDNSName(domain) {
		fail(w, http.StatusBadRequest, "请输入有效的完整域名。国际化域名请使用 Punycode。")
		return
	}
	if input.Action != "domain_add" && input.Action != "domain_remove" {
		fail(w, http.StatusBadRequest, "无效的域名操作")
		return
	}
	if _, err := a.authenticate(actor, input.Password); err != nil {
		if errors.Is(err, errLoginBusy) {
			fail(w, http.StatusTooManyRequests, "验证请求过多，请稍后重试")
		} else {
			fail(w, http.StatusForbidden, "管理员主邮箱密码不正确")
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
	// Account creation and alias changes take this lock before checking addresses.
	if letterError(w, lockIMAPUIDs(ctx, tx)) ||
		browserSessionError(w, a.lockBrowserSession(ctx, tx, actor, browserSessionDigest(r))) {
		return
	}
	var registered bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM managed_domains WHERE name=?)`, domain).Scan(&registered)
	if letterError(w, err) {
		return
	}
	if input.Action == "domain_add" {
		if registered {
			fail(w, http.StatusConflict, "此域名已登记")
			return
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO managed_domains(name) VALUES(?)`, domain)
	} else {
		if !registered {
			fail(w, http.StatusNotFound, "此域名未在后台登记")
			return
		}
		inUse, lookupErr := a.domainHasAddresses(ctx, tx, domain)
		if letterError(w, lookupErr) {
			return
		}
		if inUse {
			fail(w, http.StatusConflict, "此域名仍有主邮箱或别名，无法移除")
			return
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM managed_domains WHERE name=?`, domain)
	}
	if letterError(w, err) {
		return
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_account_actions(actor,target,action,affected,stamp)
		VALUES(?,?,?,?,?)`, actor, domain, input.Action, 0, time.Now().Unix())
	if letterError(w, err) || letterError(w, tx.Commit()) {
		return
	}
	if input.Action == "domain_add" {
		reply(w, http.StatusCreated, map[string]string{"name": domain})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) domainHasAddresses(ctx context.Context, tx *sql.Tx, domain string) (bool, error) {
	for email := range a.accounts {
		if emailDomain(email) == domain {
			return true, nil
		}
	}
	for alias := range a.aliases {
		if emailDomain(alias) == domain {
			return true, nil
		}
	}
	pattern := "%@" + domain
	dotted := pattern + "."
	var inUse bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM managed_accounts WHERE email LIKE ? OR email LIKE ?
		UNION ALL SELECT 1 FROM managed_aliases WHERE alias LIKE ? OR alias LIKE ?
	)`, pattern, dotted, pattern, dotted).Scan(&inUse)
	return inUse, err
}

func (a *app) adminDomainDNS(w http.ResponseWriter, r *http.Request, actor string) {
	domain := strings.ToLower(r.PathValue("domain"))
	if !a.canManageDomain(actor, domain) {
		fail(w, http.StatusForbidden, "当前账号无权管理此域名")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
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
	if !slices.ContainsFunc(a.scopedDomains(actor, domains), func(item adminDomain) bool { return item.Name == domain }) {
		fail(w, http.StatusNotFound, "未配置这个邮件域名")
		return
	}
	if !validDNSName(domain) || len("_dmarc."+domain) > 253 {
		fail(w, http.StatusBadRequest, "该邮箱域无法作为 DNS 名称查询")
		return
	}
	selector := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("selector")))
	dkimName := selector + "._domainkey." + domain
	if selector != "" && (!validDNSName(selector) || len(dkimName) > 253) {
		fail(w, http.StatusBadRequest, "DKIM 选择器格式无效")
		return
	}
	select {
	case dnsCheckSlots <- struct{}{}:
		defer func() { <-dnsCheckSlots }()
	default:
		fail(w, http.StatusTooManyRequests, "DNS 查询繁忙，请稍后再试")
		return
	}

	resolver := net.DefaultResolver
	var result domainDNS
	result.DKIM.State = "skipped"
	var checks sync.WaitGroup
	checks.Go(func() {
		hosts, err := resolver.LookupMX(ctx, domain+".")
		result.MX = classifyDNS(len(hosts), err)
		if len(hosts) == 1 && hosts[0].Host == "." && hosts[0].Pref == 0 {
			result.MX.State = "null"
		}
		result.Hosts = hosts
	})
	checks.Go(func() {
		result.SPF = checkTXTDNS(ctx, resolver, domain+".", func(record string) bool {
			fields := strings.Fields(record)
			return len(fields) > 0 && strings.EqualFold(fields[0], "v=spf1")
		})
	})
	checks.Go(func() {
		result.DMARC = checkTXTDNS(ctx, resolver, "_dmarc."+domain+".", func(record string) bool {
			first, _, _ := strings.Cut(record, ";")
			key, value, _ := strings.Cut(first, "=")
			return strings.TrimSpace(key) == "v" && strings.TrimSpace(value) == "DMARC1"
		})
	})
	if selector != "" {
		checks.Go(func() {
			result.DKIM = checkTXTDNS(ctx, resolver, dkimName+".", func(record string) bool {
				for _, field := range strings.Split(record, ";") {
					key, value, _ := strings.Cut(field, "=")
					if strings.TrimSpace(key) == "p" && strings.TrimSpace(value) != "" {
						return true
					}
				}
				return false
			})
		})
	}
	checks.Wait()
	reply(w, http.StatusOK, result)
}

func checkTXTDNS(ctx context.Context, resolver *net.Resolver, name string, matches func(string) bool) dnsCheck {
	records, err := resolver.LookupTXT(ctx, name)
	count := 0
	for _, record := range records {
		if matches(record) {
			count++
		}
	}
	result := classifyDNS(count, err)
	if result.State == "present" && count > 1 {
		result.State = "multiple"
	}
	return result
}

func classifyDNS(count int, err error) dnsCheck {
	if err != nil {
		var dnsErr *net.DNSError
		if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
			return dnsCheck{State: "error"}
		}
	}
	if count == 0 {
		return dnsCheck{State: "missing"}
	}
	return dnsCheck{State: "present", Count: count}
}
