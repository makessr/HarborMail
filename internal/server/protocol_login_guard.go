package server

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
)

func (a *app) authenticateProtocol(peer net.Addr, username, password string, protocol mailProtocol) (protocolSession, error) {
	var source netip.Prefix
	if peer != nil {
		host, _, _ := net.SplitHostPort(peer.String())
		ip, err := netip.ParseAddr(host)
		if err == nil {
			ip = ip.WithZone("").Unmap()
			bits := 64
			if ip.Is4() {
				bits = 32
			}
			source = netip.PrefixFrom(ip, bits).Masked()
		}
	}
	name := ""
	for _, entry := range protocolNames {
		if entry.bit == protocol {
			name = entry.name
			break
		}
	}
	login, _ := address(strings.ToLower(strings.TrimSpace(username)))
	ctx := context.Background()
	if source.IsValid() {
		if err := a.checkClientLoginRate(ctx, source, login, name); err != nil {
			return protocolSession{}, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, protocolCheckTimeout)
	defer cancel()
	generation, _, err := a.protocolAccountState(ctx, login)
	if err != nil {
		a.recordLogin(source, login, "error", name)
		return protocolSession{}, errLoginUnavailable
	}
	owner, digest, err := a.authenticateClient(username, password, protocol)
	session := protocolSession{owner: owner, generation: generation, appDigest: digest}
	if err == nil {
		err = a.checkProtocolSession(ctx, session)
	}
	switch {
	case err == nil:
		a.recordLogin(source, owner, "success", name)
	case errors.Is(err, errLoginBusy):
		a.recordLogin(source, login, "busy", name)
	case errors.Is(err, errLoginUnavailable):
		a.recordLogin(source, login, "error", name)
	default:
		a.recordLogin(source, login, "failed", name)
	}
	return session, err
}

func (a *app) checkClientLoginRate(ctx context.Context, source netip.Prefix, login, name string) error {
	for _, account := range []string{"", login} {
		retry, err := a.clientLogin.reserve(ctx, source, account, account != "")
		if err != nil {
			a.recordLogin(source, login, "error", name)
			return err
		}
		if retry > 0 {
			a.recordLogin(source, login, "blocked", name)
			return errLoginBusy
		}
		if login == "" {
			break
		}
	}
	return nil
}
