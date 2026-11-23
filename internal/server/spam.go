package server

import (
	"bytes"
	"context"
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

type scanVerdict struct {
	spam, reject, virus, unavailable bool
}

func (scanner *rspamdScanner) check(ctx context.Context, session *delivery, raw []byte) (scanVerdict, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, scanner.endpoint, bytes.NewReader(raw))
	if err != nil {
		return scanVerdict{}, err
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
		return scanVerdict{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return scanVerdict{}, fmt.Errorf("unexpected scan status %d", resp.StatusCode)
	}
	const responseLimit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))
	if err != nil {
		return scanVerdict{}, err
	}
	if len(body) > responseLimit {
		return scanVerdict{}, errors.New("scan response is too large")
	}
	var result struct {
		Skipped bool                       `json:"is_skipped"`
		Action  string                     `json:"action"`
		Symbols map[string]json.RawMessage `json:"symbols"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return scanVerdict{}, errors.New("invalid scan response")
	}
	_, infected := result.Symbols["CLAM_VIRUS"]
	_, failed := result.Symbols["CLAM_VIRUS_FAIL"]
	verdict := scanVerdict{virus: infected, unavailable: failed || result.Skipped || result.Action == "soft reject"}
	if result.Skipped {
		return verdict, nil
	}
	switch result.Action {
	case "add header", "rewrite subject", "reject", "quarantine", "discard":
		verdict.spam = true
		verdict.reject = result.Action == "reject" || result.Action == "discard" || result.Action == "quarantine"
		return verdict, nil
	case "":
		return scanVerdict{}, errors.New("scan response has no action")
	default:
		return verdict, nil
	}
}
