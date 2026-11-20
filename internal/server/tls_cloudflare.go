package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"go.uber.org/zap"
)

type dnsACME struct {
	cache   *certmagic.Cache
	manager *certmagic.Config
	hosts   []string
	allowed map[string]bool
}

func cloudflareACMEToken() (string, error) {
	token, err := fileSetting("HARBOR_ACME_CLOUDFLARE_TOKEN")
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("Cloudflare DNS-01 requires HARBOR_ACME_CLOUDFLARE_TOKEN or HARBOR_ACME_CLOUDFLARE_TOKEN_FILE")
	}
	return token, nil
}

func newCloudflareACME(hosts []string, allowed map[string]bool, cacheDir, directory, email, token string) (*dnsACME, error) {
	logger, err := zap.NewProduction()
	if err != nil {
		return nil, fmt.Errorf("configure ACME logger: %w", err)
	}
	logger = logger.Named("harbor_acme")
	dns := &dnsACME{hosts: hosts, allowed: allowed}
	dns.cache = certmagic.NewCache(certmagic.CacheOptions{
		Logger: logger,
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return dns.manager, nil
		},
	})
	dns.manager = certmagic.New(dns.cache, certmagic.Config{
		Storage: &certmagic.FileStorage{Path: filepath.Join(cacheDir, "cloudflare")},
		Logger:  logger,
	})
	if directory == "" {
		directory = certmagic.LetsEncryptProductionCA
	}
	issuer := certmagic.NewACMEIssuer(dns.manager, certmagic.ACMEIssuer{
		CA: directory, TestCA: directory, Email: email, Agreed: true, Logger: logger,
		DisableHTTPChallenge: true, DisableTLSALPNChallenge: true,
		DNS01Solver: &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{
			DNSProvider: &cloudflare.Provider{APIToken: token, HTTPClient: &http.Client{Timeout: 30 * time.Second}},
		}},
	})
	dns.manager.Issuers = []certmagic.Issuer{issuer}
	return dns, nil
}

func (d *dnsACME) start(ctx context.Context) error {
	if err := d.manager.ManageAsync(ctx, d.hosts); err != nil {
		return fmt.Errorf("manage Cloudflare DNS-01 certificates: %w", err)
	}
	return nil
}

func (d *dnsACME) certificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	client := *hello
	client.ServerName = normalizeACMEHost(client.ServerName)
	if client.ServerName == "" {
		client.ServerName = d.hosts[0]
	}
	if !d.allowed[client.ServerName] {
		return nil, errors.New("TLS hostname is not configured for ACME")
	}
	return d.manager.GetCertificate(&client)
}
