package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type rspamdScanner struct {
	endpoint string
	client   *http.Client
}

func newRspamdScanner(endpoint string) (*rspamdScanner, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "/checkv2" {
		return nil, errors.New("HARBOR_RSPAMD_URL must be an HTTP(S) /checkv2 endpoint without credentials, query or fragment")
	}
	return &rspamdScanner{endpoint: u.String(), client: &http.Client{
		Timeout:   4 * time.Second,
		Transport: &http.Transport{MaxConnsPerHost: 8, MaxIdleConnsPerHost: 8, IdleConnTimeout: 30 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}, nil
}

func (scanner *rspamdScanner) check(session *delivery, raw []byte) (bool, error) {
	req, err := http.NewRequest(http.MethodPost, scanner.endpoint, bytes.NewReader(raw))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "message/rfc822")
	sender := session.sender
	if sender == "" {
		sender = "<>"
	}
	req.Header.Set("From", sender)
	for _, recipient := range session.recipients {
		req.Header.Add("Rcpt", recipient.address)
	}
	if conn := session.conn; conn != nil {
		if ip, _, err := net.SplitHostPort(conn.Conn().RemoteAddr().String()); err == nil {
			req.Header.Set("IP", ip)
		}
		if helo := conn.Hostname(); helo != "" {
			req.Header.Set("Helo", helo)
		}
	}
	resp, err := scanner.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected scan status %d", resp.StatusCode)
	}
	const responseLimit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))
	if err != nil {
		return false, err
	}
	if len(body) > responseLimit {
		return false, errors.New("scan response is too large")
	}
	var result struct {
		Skipped bool   `json:"is_skipped"`
		Action  string `json:"action"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return false, errors.New("invalid scan response")
	}
	if result.Skipped {
		return false, nil
	}
	switch result.Action {
	case "add header", "rewrite subject", "reject", "quarantine", "discard":
		return true, nil
	case "":
		return false, errors.New("scan response has no action")
	default:
		return false, nil
	}
}
