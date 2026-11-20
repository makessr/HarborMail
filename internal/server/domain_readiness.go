package server

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type domainCheckStep struct {
	State string `json:"state"`
	Next  string `json:"next,omitempty"`
	Until int64  `json:"until,omitempty"`
}

type domainReadiness struct {
	domainDNS
	Key   *dkimCheckReport `json:"key,omitempty"`
	TLS   domainCheckStep  `json:"tls"`
	Mode  string           `json:"outboundMode"`
	Relay domainCheckStep  `json:"relay"`
}

func (a *app) checkDomainReadiness(ctx context.Context, domain, selector string) domainReadiness {
	var result domainReadiness
	var checks sync.WaitGroup
	checks.Go(func() {
		result.domainDNS = lookupDomainDNS(ctx, net.DefaultResolver, domain, selector)
	})
	checks.Go(func() {
		report, err := inspectDKIM(ctx, a.db, net.DefaultResolver, a.dkimCipher, nil, domain, "active")
		if err == nil && report.KeyState != "not_configured" {
			result.Key = &report
		}
	})
	checks.Go(func() {
		host := strings.ToLower(strings.TrimSpace(env("HARBOR_PUBLIC_HTTPS_HOST", publicClientHost())))
		port, err := strconv.Atoi(env("HARBOR_PUBLIC_HTTPS_PORT", "443"))
		if !strings.Contains(host, ".") || !validDNSName(host) || err != nil || port < 1 || port > 65535 {
			result.TLS = domainCheckStep{State: "skipped", Next: "配置公网主机名和 HTTPS 端口后再检查。"}
			return
		}
		dialer := tls.Dialer{Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}}
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			result.TLS = domainCheckStep{State: "error", Next: "从部署网络核对公网地址、端口和证书链。"}
			return
		}
		defer conn.Close()
		cert := conn.(*tls.Conn).ConnectionState().PeerCertificates[0]
		result.TLS = domainCheckStep{State: "pass", Until: cert.NotAfter.Unix()}
	})
	checks.Go(func() {
		sender := "postmaster@" + domain
		if !a.hasRelayFor(sender) {
			result.Mode = "local"
			result.Relay = domainCheckStep{State: "skipped", Next: "当前仅支持站内投递。"}
			if a.directMX != nil {
				result.Mode = "direct"
				result.Relay = domainCheckStep{State: "configured", Next: "已启用 MX 直投，向外部邮箱发送测试邮件确认实际投递。"}
			}
			return
		}
		result.Mode = "relay"
		client, closeClient, err := a.connectSenderRelay(ctx, sender)
		if err == nil {
			defer closeClient()
			err = client.Noop()
		}
		if err != nil {
			result.Relay = domainCheckStep{State: "error", Next: relayCheckError(ctx, err).Error()}
			return
		}
		result.Relay = domainCheckStep{State: "pass"}
	})
	checks.Wait()
	return result
}

func (a *app) adminVerifyDomainMail(w http.ResponseWriter, r *http.Request, actor string) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	domain := strings.ToLower(r.PathValue("domain"))
	if !a.checkAdminDNSDomain(w, ctx, actor, domain) {
		return
	}
	selector := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("selector")))
	if selector != "" && (!validDNSName(selector) || len(selector+"._domainkey."+domain) > 253) {
		fail(w, http.StatusBadRequest, "DKIM 选择器格式无效")
		return
	}
	select {
	case dnsCheckSlots <- struct{}{}:
		defer func() { <-dnsCheckSlots }()
	default:
		fail(w, http.StatusTooManyRequests, "验证繁忙，请稍后再试")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDKIMVerifyMessage))
	if err != nil || len(raw) == 0 {
		fail(w, http.StatusBadRequest, "请选择 32 MiB 以内的 EML 原件")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	reply(w, http.StatusOK, dkimVerifyReport{domain, selector, verifyReceivedDKIM(ctx, net.DefaultResolver, raw, domain, selector)})
}
