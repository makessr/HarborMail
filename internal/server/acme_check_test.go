package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type acmeCheckTransport func(*http.Request) (*http.Response, error)

func (f acmeCheckTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCheckCloudflareZone(t *testing.T) {
	const token = "synthetic-acme-check-token"
	const zone = "harbor.test"
	const txt = "_acme-challenge.mail.harbor.test"
	active := `{"success":true,"result":[{"id":"zone-id","name":"harbor.test","status":"active"}]}`
	for _, tc := range []struct {
		name, zoneBody, recordsBody, want string
		recordsStatus, requests           int
	}{
		{"empty TXT list", active, `{"success":true,"result":[]}`, "", 200, 2},
		{"token cannot read the DNS records", active, token, "DNS record lookup failed: Cloudflare API HTTP 403", 403, 2},
		{"missing zone", `{"success":true,"result":[]}`, "", "zone not found", 0, 1},
		{"pending zone", strings.Replace(active, "active", "pending", 1), "", "zone is not active", 0, 1},
		{"invalid result", active, `{"success":true,"result":null}`, "did not return a DNS record list", 200, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: acmeCheckTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != "GET" || req.URL.Host != "api.cloudflare.com" || req.Header.Get("Authorization") != "Bearer "+token {
					t.Fatal("unexpected Cloudflare request")
				}
				status, body := 200, tc.zoneBody
				query := req.URL.Query()
				switch req.URL.Path {
				case "/client/v4/zones":
					if query.Get("name") != zone {
						t.Fatal("unexpected zone filter")
					}
				case "/client/v4/zones/zone-id/dns_records":
					if query.Get("name.exact") != txt || query.Get("type") != "TXT" {
						t.Fatal("unexpected DNS record filters")
					}
					status, body = tc.recordsStatus, tc.recordsBody
				default:
					t.Fatal("unexpected Cloudflare API endpoint")
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			err := checkCloudflareZone(context.Background(), client, token, zone, txt)
			switch {
			case tc.want == "" && err != nil:
				t.Fatal(err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), token)):
				t.Fatalf("unexpected diagnostic: %v", err)
			}
			if calls != tc.requests {
				t.Fatalf("expected %d read requests, got %d", tc.requests, calls)
			}
		})
	}
}
