package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
)

const cloudflareAPI = "https://api.cloudflare.com/client/v4"

func runACMECheck(out io.Writer) error {
	if os.Getenv("HARBOR_ACME_CHALLENGE") != "cloudflare" {
		return errors.New("acme-check requires HARBOR_ACME_CHALLENGE=cloudflare")
	}
	hosts, _, err := parseACMEHosts(os.Getenv("HARBOR_ACME_HOSTS"))
	if err != nil {
		return err
	}
	token, err := cloudflareACMEToken()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client := &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for _, host := range hosts {
		txt := "_acme-challenge." + host
		zone, err := certmagic.FindZoneByFQDN(ctx, zap.NewNop(), txt, certmagic.RecursiveNameservers(nil))
		if err != nil {
			return fmt.Errorf("%s: cannot resolve the DNS zone for %s: %w", host, txt, err)
		}
		zone = normalizeACMEHost(zone)
		if err := checkCloudflareZone(ctx, client, token, zone, txt); err != nil {
			return fmt.Errorf("%s (zone %s): %w", host, zone, err)
		}
		if _, err := fmt.Fprintf(out, "%s: Cloudflare zone %s and DNS read access verified\n", host, zone); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(out, "DNS write access and certificate issuance are verified during issuance.")
	return err
}

func checkCloudflareZone(ctx context.Context, client *http.Client, token, zone, txt string) error {
	var zones []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	query := url.Values{"name": {zone}}
	if err := readCloudflareResult(ctx, client, token, "/zones", query, &zones); err != nil {
		return fmt.Errorf("zone lookup failed: %w", err)
	}
	if len(zones) != 1 || zones[0].Name != zone || zones[0].ID == "" {
		return errors.New("zone not found with this token; check nameservers and Zone Read scope")
	}
	if zones[0].Status != "active" {
		return errors.New("Cloudflare zone is not active")
	}
	query = url.Values{"name.exact": {txt}, "type": {"TXT"}, "per_page": {"5"}}
	var records []json.RawMessage
	path := "/zones/" + url.PathEscape(zones[0].ID) + "/dns_records"
	if err := readCloudflareResult(ctx, client, token, path, query, &records); err != nil {
		return fmt.Errorf("DNS record lookup failed: %w", err)
	}
	if records == nil {
		return errors.New("Cloudflare API did not return a DNS record list")
	}
	return nil
}

func readCloudflareResult(ctx context.Context, client *http.Client, token, path string, query url.Values, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cloudflareAPI+path+"?"+query.Encode(), nil)
	if err != nil {
		return errors.New("cannot build Cloudflare API request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("cannot reach Cloudflare API; check the network and the API token format")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("Cloudflare API HTTP %d; check token validity and permissions", resp.StatusCode)
	case http.StatusOK:
	default:
		return fmt.Errorf("Cloudflare API HTTP %d", resp.StatusCode)
	}
	var result struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&result); err != nil || !result.Success {
		return errors.New("Cloudflare API did not return a successful result")
	}
	if err := json.Unmarshal(result.Result, dst); err != nil {
		return errors.New("Cloudflare API returned an invalid result")
	}
	return nil
}
