package server

import (
	"bytes"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func webHealthcheck() error {
	host, port, err := net.SplitHostPort(env("HARBOR_WEB", "127.0.0.1:8080"))
	if err != nil {
		return fmt.Errorf("invalid HARBOR_WEB address: %w", err)
	}
	switch host {
	case "", "0.0.0.0", "localhost":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	if ip := net.ParseIP(host); !ip.IsLoopback() {
		return errors.New("the Web healthcheck requires a loopback or wildcard HARBOR_WEB address")
	}
	transport := &http.Transport{}
	scheme := "http"
	if os.Getenv("HARBOR_WEB_TLS") != "off" {
		if hosts := os.Getenv("HARBOR_ACME_HOSTS"); hosts != "" {
			first, _, _ := strings.Cut(hosts, ",")
			transport.TLSClientConfig = &tls.Config{
				MinVersion: tls.VersionTLS12, ServerName: normalizeACMEHost(strings.TrimSpace(first)),
			}
			scheme = "https"
		} else if certFile := os.Getenv("HARBOR_CERT"); certFile != "" {
			config, err := healthcheckPEMTLS(certFile)
			if err != nil {
				return err
			}
			transport.TLSClientConfig = config
			scheme = "https"
		}
	}

	client := &http.Client{
		Transport:     transport,
		Timeout:       4 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer transport.CloseIdleConnections()
	response, err := client.Get(scheme + "://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Web healthcheck returned HTTP %d", response.StatusCode)
	}
	return nil
}

func healthcheckPEMTLS(path string) (*tls.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read healthcheck certificate: %w", err)
	}
	for {
		block, remaining := pem.Decode(data)
		if block == nil {
			return nil, errors.New("healthcheck PEM has no certificate")
		}
		if block.Type != "CERTIFICATE" {
			data = remaining
			continue
		}
		return &tls.Config{
			MinVersion: tls.VersionTLS12,
			// The loopback endpoint presents the exact configured leaf, which
			// may be self-signed and may have only a public hostname.
			InsecureSkipVerify: true,
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 || !bytes.Equal(state.PeerCertificates[0].Raw, block.Bytes) {
					return errors.New("Web healthcheck TLS certificate does not match HARBOR_CERT")
				}
				cert, now := state.PeerCertificates[0], time.Now()
				if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
					return errors.New("Web healthcheck TLS certificate is outside its validity period")
				}
				return nil
			},
		}, nil
	}
}
