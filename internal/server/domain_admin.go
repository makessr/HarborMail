package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
)

func parseDomainAdmins(raw string, accounts map[string][]byte) (map[string]map[string]bool, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var settings map[string][]string
	if err := json.Unmarshal([]byte(raw), &settings); err != nil || settings == nil {
		return nil, errors.New("HARBOR_DOMAIN_ADMINS must contain a JSON object")
	}
	result := make(map[string]map[string]bool)
	for owner, domains := range settings {
		if _, ok := accounts[owner]; !ok || len(domains) == 0 || len(domains) > 100 {
			return nil, errors.New("each domain administrator must be a configured mailbox with at least one domain")
		}
		scope := make(map[string]bool)
		for _, domain := range domains {
			if !strings.Contains(domain, ".") || strings.ToLower(domain) != domain || !validDNSName(domain) {
				return nil, errors.New("invalid domain administrator scope")
			}
			scope[domain] = true
		}
		result[owner] = scope
	}
	return result, nil
}

func (a *app) domainAdminOnly(handler authorized) http.HandlerFunc {
	return a.auth(func(w http.ResponseWriter, r *http.Request, actor string) {
		administrator, err := a.isGlobalAdmin(r.Context(), a.db, actor)
		if letterError(w, err) {
			return
		}
		if !administrator && len(a.domainAdmins[actor]) == 0 {
			fail(w, http.StatusForbidden, "当前账号没有管理权限")
			return
		}
		handler(w, r, actor)
	})
}

func (a *app) canManageDomain(ctx context.Context, actor, domain string) bool {
	administrator, err := a.isGlobalAdmin(ctx, a.db, actor)
	return err == nil && (administrator || a.domainAdmins[actor][domain])
}

func (a *app) canManageDomainAlias(actor, alias, owner string, admins map[string]bool) bool {
	return admins[actor] || (!admins[owner] && a.domainAdmins[actor][emailDomain(alias)] && a.domainAdmins[actor][emailDomain(owner)])
}

func (a *app) scopedDomains(actor string, all []adminDomain, administrator bool) []adminDomain {
	if administrator {
		return all
	}
	byName := make(map[string]adminDomain)
	for _, domain := range all {
		byName[domain.Name] = domain
	}
	visible := make([]adminDomain, 0, len(a.domainAdmins[actor]))
	for name := range a.domainAdmins[actor] {
		domain := byName[name]
		domain.Name = name
		visible = append(visible, domain)
	}
	slices.SortFunc(visible, func(left, right adminDomain) int { return strings.Compare(left.Name, right.Name) })
	return visible
}

func parseDomainQuotaDefaults(raw string) (map[string]int64, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var settings map[string]int64
	if err := json.Unmarshal([]byte(raw), &settings); err != nil || settings == nil {
		return nil, errors.New("HARBOR_DOMAIN_DEFAULT_QUOTAS must contain a JSON object")
	}
	for domain, limit := range settings {
		if !validDNSName(domain) || strings.ToLower(domain) != domain || limit < 0 || limit > maxStorageThresholdMiB {
			return nil, errors.New("invalid domain mailbox quota default")
		}
	}
	return settings, nil
}
