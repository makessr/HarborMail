package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

const maxDKIMVerifyMessage = 32 << 20

type dkimVerifyReport struct {
	Domain   string `json:"domain"`
	Selector string `json:"selector,omitempty"`
	Status   string `json:"status"`
}

func runDKIMVerify(in io.Reader, out io.Writer, resolver *net.Resolver, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: harbormail dkim-verify DOMAIN [SELECTOR] < received.eml")
	}
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(args[0])), ".")
	if !strings.Contains(domain, ".") || !validDNSName(domain) || len("a._domainkey."+domain) > 253 {
		return errors.New("provide the fully qualified DKIM signing domain; use Punycode for internationalized domains")
	}
	report := dkimVerifyReport{Domain: domain}
	if len(args) == 2 {
		report.Selector = strings.ToLower(strings.TrimSpace(args[1]))
		if !validDNSName(report.Selector) || len(report.Selector+"._domainkey."+domain) > 253 {
			return errors.New("invalid DKIM selector")
		}
	}
	// Buffer a bounded input so a read error cannot be confused with an invalid signature.
	raw, err := io.ReadAll(io.LimitReader(in, maxDKIMVerifyMessage+1))
	if err != nil {
		return errors.New("could not read the received message from standard input")
	}
	if len(raw) > maxDKIMVerifyMessage {
		return errors.New("the received message exceeds 32 MiB")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	report.Status = verifyReceivedDKIM(ctx, resolver, raw, domain, report.Selector)
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return err
	}
	if report.Status != "pass" {
		return errors.New("DKIM verification did not pass; see status in the report")
	}
	return nil
}

func verifyReceivedDKIM(ctx context.Context, resolver *net.Resolver, raw []byte, domain, selector string) string {
	results, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{
		MaxVerifications: 16,
		LookupTXT: func(name string) ([]string, error) {
			name = strings.ToLower(name)
			label, signedDomain, ok := strings.Cut(name, "._domainkey.")
			if !ok || signedDomain != domain || !validDNSName(label) || len(name) > 253 || (selector != "" && label != selector) {
				return nil, errors.New("signature is outside the requested DKIM identity")
			}
			return resolver.LookupTXT(ctx, name+".")
		},
	})
	if errors.Is(err, dkim.ErrTooManySignatures) {
		return "too_many_signatures"
	}
	if err != nil {
		return "invalid_message"
	}
	state := "no_signature"
	for _, result := range results {
		if !strings.EqualFold(result.Domain, domain) {
			continue
		}
		if result.Err == nil {
			return "pass"
		}
		if dkim.IsTempFail(result.Err) {
			state = "temporary_error"
		} else if state == "no_signature" {
			state = "fail"
		}
	}
	return state
}
