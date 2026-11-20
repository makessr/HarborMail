package server

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

type mailDNSRecord struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Value    string `json:"value,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Purpose  string `json:"purpose"`
}

func (a *app) adminDomainDNSPlan(w http.ResponseWriter, r *http.Request, actor string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	domain := strings.ToLower(r.PathValue("domain"))
	if !a.checkAdminDNSDomain(w, ctx, actor, domain) {
		return
	}
	query := r.URL.Query()
	requestedHost := strings.TrimSpace(query.Get("host"))
	host := dnsPlanName(requestedHost)
	if requestedHost != "" && !validMailDNSHost(host) {
		fail(w, http.StatusBadRequest, "邮件主机名须为完整 DNS 名称，例如 mail.example.com")
		return
	}
	if host == "" {
		if configured := publicClientHost(); validMailDNSHost(configured) {
			host = configured
		} else if validMailDNSHost("mail." + domain) {
			host = "mail." + domain
		}
	}
	ipv4, ok4 := dnsPlanAddress(query.Get("ipv4"), true)
	ipv6, ok6 := dnsPlanAddress(query.Get("ipv6"), false)
	if !ok4 || !ok6 {
		fail(w, http.StatusBadRequest, "请填写对应的公网 IPv4 或 IPv6，或留空")
		return
	}
	include := strings.TrimPrefix(dnsPlanName(query.Get("spfInclude")), "include:")
	if strings.TrimSpace(query.Get("spfInclude")) != "" && !validMailDNSHost(strings.ReplaceAll(include, "_", "a")) {
		fail(w, http.StatusBadRequest, "请填写中继提供的 SPF include 域名")
		return
	}
	status, err := a.readDKIMStatus(ctx, domain)
	if letterError(w, err) {
		return
	}
	records := make([]mailDNSRecord, 0, 7)
	if ipv4 != "" || ipv6 == "" {
		records = append(records, mailDNSRecord{Type: "A", Name: host, Value: ipv4, Purpose: "邮件主机 IPv4"})
	}
	if ipv6 != "" {
		records = append(records, mailDNSRecord{Type: "AAAA", Name: host, Value: ipv6, Purpose: "邮件主机 IPv6"})
	}
	spf := ""
	if query.Get("receiveOnly") == "1" {
		spf = "v=spf1 -all"
	} else if include != "" {
		spf = "v=spf1 include:" + include + " ~all"
	}
	records = append(records,
		mailDNSRecord{Type: "MX", Name: domain, Value: host, Priority: 10, Purpose: "接收邮件"},
		mailDNSRecord{Type: "TXT", Name: domain, Value: spf, Purpose: "SPF"},
		mailDNSRecord{Type: "TXT", Name: "_dmarc." + domain, Value: "v=DMARC1; p=none", Purpose: "DMARC 初始策略"},
	)
	for _, key := range []struct {
		public  *dkimPublicKey
		purpose string
	}{
		{status.Active, "DKIM 已启用"},
		{status.Pending, "DKIM 待启用"},
	} {
		if key.public != nil {
			records = append(records, mailDNSRecord{Type: "TXT", Name: key.public.Name, Value: key.public.TXT, Purpose: key.purpose})
		}
	}
	if status.Active == nil && status.Pending == nil {
		records = append(records, mailDNSRecord{Type: "TXT", Purpose: "DKIM"})
	}
	w.Header().Set("Cache-Control", "no-store")
	reply(w, http.StatusOK, map[string]any{"host": host, "records": records, "dkimAvailable": status.Available && !a.recoveryMode && validDKIMDomain(domain), "dkimMissing": status.Active == nil && status.Pending == nil})
}

func dnsPlanName(raw string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
}

func validMailDNSHost(host string) bool {
	_, ipErr := netip.ParseAddr(host)
	return ipErr != nil && strings.Contains(host, ".") && validDNSName(host)
}

func dnsPlanAddress(raw string, v4 bool) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	ip, err := netip.ParseAddr(raw)
	if err != nil || ip.Is4() != v4 || ip.Is4In6() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return "", false
	}
	return ip.String(), true
}
