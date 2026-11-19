package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

type domainRelay struct {
	Address  string `json:"address"`
	TLS      string `json:"tls"`
	User     string `json:"user"`
	Password string `json:"password"`
}

var errRelayAuthentication = errors.New("SMTP relay authentication failed")

func (relay domainRelay) validate() error {
	host, port, err := net.SplitHostPort(relay.Address)
	number, portErr := strconv.Atoi(port)
	if err != nil || host == "" || portErr != nil || number < 1 || number > 65535 {
		return errors.New("relay address must contain a host and a valid port")
	}
	if relay.TLS != "starttls" && relay.TLS != "implicit" {
		return errors.New("relay TLS mode must be starttls or implicit")
	}
	if (relay.User == "") != (relay.Password == "") || strings.ContainsRune(relay.User+relay.Password, 0) {
		return errors.New("relay user and password must be configured together and contain no NUL bytes")
	}
	return nil
}

func parseDomainRelays(raw string) (map[string]domainRelay, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var relays map[string]domainRelay
	if err := json.Unmarshal([]byte(raw), &relays); err != nil || relays == nil {
		return nil, errors.New("HARBOR_DOMAIN_RELAYS must contain a JSON object")
	}
	for domain, relay := range relays {
		if !validDNSName(domain) || strings.ToLower(domain) != domain {
			return nil, errors.New("invalid per-domain relay sender domain")
		}
		if err := relay.validate(); err != nil {
			return nil, fmt.Errorf("invalid relay for %s: %w", domain, err)
		}
	}
	return relays, nil
}

func (a *app) sendRelay(owner string) domainRelay {
	if relay, exists := a.domainRelays[emailDomain(owner)]; exists {
		return relay
	}
	return a.relay
}

func (a *app) hasRelayFor(owner string) bool {
	return a.sendRelay(owner).Address != ""
}

func (a *app) connectSenderRelay(ctx context.Context, owner string) (*smtp.Client, func(), error) {
	relay := a.sendRelay(owner)
	host, _, err := net.SplitHostPort(relay.Address)
	if err != nil {
		return nil, nil, errors.New("no external relay is configured for the sender")
	}
	cfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	client, closeClient, err := dialRelay(ctx, relay.Address, relay.TLS == "implicit", cfg)
	if err != nil {
		return nil, nil, err
	}
	if relay.User != "" {
		if err := client.Auth(sasl.NewPlainClient("", relay.User, relay.Password)); err != nil {
			closeClient()
			return nil, nil, fmt.Errorf("%w: %w", errRelayAuthentication, err)
		}
	}
	return client, closeClient, nil
}
