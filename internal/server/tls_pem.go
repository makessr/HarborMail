package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"time"
)

const pemReloadInterval = 5 * time.Second

type pemTLS struct {
	certFile    string
	keyFile     string
	active      atomic.Pointer[tls.Certificate]
	fingerprint [2][sha256.Size]byte
}

func (p *pemTLS) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return p.active.Load(), nil
}

func (p *pemTLS) reload() (bool, error) {
	certPEM, err := os.ReadFile(p.certFile)
	if err != nil {
		return false, fmt.Errorf("read TLS certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(p.keyFile)
	if err != nil {
		return false, fmt.Errorf("read TLS private key: %w", err)
	}
	fingerprint := [2][sha256.Size]byte{sha256.Sum256(certPEM), sha256.Sum256(keyPEM)}
	if fingerprint == p.fingerprint {
		return false, nil
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return false, err
	}
	if pair.Leaf == nil {
		pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return false, err
		}
	}
	if p.active.Load() != nil {
		now := time.Now()
		if now.Before(pair.Leaf.NotBefore) || !now.Before(pair.Leaf.NotAfter) {
			return false, errors.New("replacement TLS certificate is outside its validity period")
		}
		for _, der := range pair.Certificate[1:] {
			if _, err := x509.ParseCertificate(der); err != nil {
				return false, fmt.Errorf("parse replacement TLS certificate chain: %w", err)
			}
		}
	}
	p.active.Store(&pair)
	p.fingerprint = fingerprint
	return true, nil
}

func (p *pemTLS) watch(ctx context.Context) {
	ticker := time.NewTicker(pemReloadInterval)
	defer ticker.Stop()
	var lastFailure string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed, err := p.reload()
			if err != nil {
				if message := err.Error(); message != lastFailure {
					log.Printf("reload TLS certificate: %s; keeping the active certificate", message)
					lastFailure = message
				}
				continue
			}
			lastFailure = ""
			if changed {
				log.Print("Reloaded TLS certificate")
			}
		}
	}
}
