package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

type managedTLS struct {
	manager *autocert.Manager
	hosts   []string
}

type inboundTLS struct {
	config *tls.Config
	acme   *managedTLS
	pem    *pemTLS
}

var acmeDNSLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func configureTLS() (inboundTLS, error) {
	certFile, keyFile := os.Getenv("HARBOR_CERT"), os.Getenv("HARBOR_KEY")
	hostList := os.Getenv("HARBOR_ACME_HOSTS")
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if hostList == "" {
		if certFile == "" && keyFile == "" {
			return inboundTLS{}, nil
		}
		pem := &pemTLS{certFile: certFile, keyFile: keyFile}
		if _, err := pem.reload(); err != nil {
			return inboundTLS{}, err
		}
		config.GetCertificate = pem.getCertificate
		return inboundTLS{config: config, pem: pem}, nil
	}
	if certFile != "" || keyFile != "" {
		return inboundTLS{}, errors.New("HARBOR_ACME_HOSTS cannot be combined with HARBOR_CERT or HARBOR_KEY")
	}
	if os.Getenv("HARBOR_ACME_ACCEPT_TOS") != "true" {
		return inboundTLS{}, errors.New("automatic certificates require HARBOR_ACME_ACCEPT_TOS=true")
	}

	allowed := make(map[string]bool)
	var hosts []string
	for _, name := range strings.Split(hostList, ",") {
		host := normalizeACMEHost(strings.TrimSpace(name))
		if len(host) > 253 || !strings.Contains(host, ".") || net.ParseIP(host) != nil {
			return inboundTLS{}, errors.New("HARBOR_ACME_HOSTS must contain DNS hostnames")
		}
		for _, label := range strings.Split(host, ".") {
			if !acmeDNSLabel.MatchString(label) {
				return inboundTLS{}, errors.New("HARBOR_ACME_HOSTS must contain ASCII DNS hostnames")
			}
		}
		if !allowed[host] {
			allowed[host] = true
			hosts = append(hosts, host)
		}
	}
	directory := os.Getenv("HARBOR_ACME_DIRECTORY")
	if directory != "" {
		parsed, err := url.Parse(directory)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
			return inboundTLS{}, errors.New("HARBOR_ACME_DIRECTORY must be an HTTPS directory URL")
		}
	}
	cache := env("HARBOR_ACME_CACHE", "data/acme")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return inboundTLS{}, fmt.Errorf("create ACME cache: %w", err)
	}
	if err := checkDataDir(cache); err != nil {
		return inboundTLS{}, fmt.Errorf("HARBOR_ACME_CACHE is not writable: %w", err)
	}
	manager := &autocert.Manager{
		Prompt: autocert.AcceptTOS,
		Email:  os.Getenv("HARBOR_ACME_EMAIL"),
		Cache:  autocert.DirCache(cache),
		Client: &acme.Client{DirectoryURL: directory},
		HostPolicy: func(_ context.Context, name string) error {
			if host, _, err := net.SplitHostPort(name); err == nil {
				name = host
			}
			if !allowed[normalizeACMEHost(name)] {
				return errors.New("ACME hostname is not configured")
			}
			return nil
		},
	}
	managed := &managedTLS{manager: manager, hosts: hosts}
	config.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		client := *hello
		for _, protocol := range client.SupportedProtos {
			if protocol == acme.ALPNProto {
				return nil, errors.New("ACME TLS challenge must use the web listener")
			}
		}
		if client.ServerName == "" {
			client.ServerName = hosts[0]
		}
		return managed.certificate(&client)
	}
	return inboundTLS{config: config, acme: managed}, nil
}

func normalizeACMEHost(name string) string {
	return strings.TrimSuffix(strings.ToLower(name), ".")
}

func (m *managedTLS) certificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	client := *hello
	client.ServerName = normalizeACMEHost(client.ServerName)
	// Check SNI before autocert, which can serve cached certificates without HostPolicy.
	for _, host := range m.hosts {
		if host == client.ServerName {
			return m.manager.GetCertificate(&client)
		}
	}
	return nil, errors.New("TLS hostname is not configured for ACME")
}

func (m *managedTLS) listenChallenge(runtime *serviceRuntime) error {
	addr := env("HARBOR_ACME_HTTP", "127.0.0.1:8081")
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen for ACME HTTP challenge: %w", err)
	}
	server := &http.Server{
		Handler:           m.manager.HTTPHandler(http.NotFoundHandler()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	log.Printf("ACME HTTP challenge listening on %s", listener.Addr())
	runtime.serveHTTP("ACME HTTP challenge", server, listener)
	return nil
}

func (m *managedTLS) warmCertificates(ctx context.Context) {
	for _, host := range m.hosts {
		go func() {
			// Start the renewal timer even when the host receives no TLS traffic.
			hello := &tls.ClientHelloInfo{
				ServerName:       host,
				CipherSuites:     []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
				SupportedCurves:  []tls.CurveID{tls.CurveP256},
				SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
			}
			timer := time.NewTimer(30 * time.Minute)
			defer timer.Stop()
			for ctx.Err() == nil {
				_, err := m.certificate(hello)
				if err == nil {
					log.Printf("ACME certificate ready for %s", host)
					return
				}
				log.Printf("prepare ACME certificate for %s: %v", host, err)
				timer.Reset(30 * time.Minute)
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
				}
			}
		}()
	}
}
