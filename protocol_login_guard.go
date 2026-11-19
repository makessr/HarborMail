package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
)

func (a *app) authenticateProtocol(peer net.Addr, username, password string, protocol mailProtocol) (string, error) {
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
	if source.IsValid() && (a.clientLogin.reserve(ctx, source, "", false) > 0 ||
		(login != "" && a.clientLogin.reserve(ctx, source, login, true) > 0)) {
		a.webLogin.recordProtocol(source, "", "blocked", name)
		return "", errLoginBusy
	}
	owner, err := a.authenticateClient(username, password, protocol)
	switch {
	case err == nil:
		a.webLogin.recordProtocol(source, owner, "success", name)
	case errors.Is(err, errLoginBusy):
		a.webLogin.recordProtocol(source, "", "busy", name)
	default:
		a.webLogin.recordProtocol(source, "", "failed", name)
	}
	return owner, err
}
