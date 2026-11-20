package server

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

type clientProtocol struct {
	Name string `json:"name"`
	TLS  string `json:"tls"`
	Port int    `json:"port"`
}

func publicClientHost() string {
	host := strings.ToLower(strings.TrimSpace(os.Getenv("HARBOR_CLIENT_HOST")))
	if !strings.Contains(host, ".") || !validDNSName(host) {
		return ""
	}
	return host
}

func (a *app) clientSettings(w http.ResponseWriter, _ *http.Request, owner string) {
	host := publicClientHost()
	protocols := make([]clientProtocol, 0, 4)
	for _, protocol := range []struct{ name, tls, listener, public string }{
		{"IMAP", "SSL/TLS", "HARBOR_IMAPS", "HARBOR_IMAPS_PORT"},
		{"POP3", "SSL/TLS", "HARBOR_POP3S", "HARBOR_POP3S_PORT"},
		{"SMTP", "STARTTLS", "HARBOR_SUBMISSION", "HARBOR_SUBMISSION_PORT"},
		{"SMTP", "SSL/TLS", "HARBOR_SUBMISSIONS", "HARBOR_SMTPS_PORT"},
	} {
		listener := os.Getenv(protocol.listener)
		if host == "" || listener == "" {
			continue
		}
		_, localPort, _ := net.SplitHostPort(listener)
		port, err := strconv.Atoi(env(protocol.public, localPort))
		if err == nil && port >= 1 && port <= 65535 {
			protocols = append(protocols, clientProtocol{protocol.name, protocol.tls, port})
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	reply(w, http.StatusOK, map[string]any{"host": host, "username": owner, "protocols": protocols})
}
