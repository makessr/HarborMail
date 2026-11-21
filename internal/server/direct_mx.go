package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	stdsmtp "net/smtp"
	"net/textproto"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/emersion/go-smtp"
)

type outboundSMTP struct {
	mail func(string) error
	rcpt func(string) error
	data func() (io.WriteCloser, error)
}

type recipientConnector func(context.Context, string) (outboundSMTP, func(), error)

func relayForRecipient(connect relayConnector) recipientConnector {
	return func(ctx context.Context, _ string) (outboundSMTP, func(), error) {
		client, closeClient, err := connect(ctx)
		if err != nil {
			return outboundSMTP{}, nil, err
		}
		return outboundSMTP{
			mail: func(from string) error { return client.Mail(from, nil) },
			rcpt: func(to string) error { return client.Rcpt(to, nil) },
			data: func() (io.WriteCloser, error) { return client.Data() },
		}, closeClient, nil
	}
}

func smtpResponseCode(err error) int {
	var relay *smtp.SMTPError
	if errors.As(err, &relay) {
		return relay.Code
	}
	var direct *textproto.Error
	if errors.As(err, &direct) {
		return direct.Code
	}
	return 0
}

type mxResolver interface {
	LookupMX(context.Context, string) ([]*net.MX, error)
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type directMX struct {
	helo       string
	requireTLS bool
	resolver   mxResolver
	dial       func(context.Context, string, string) (net.Conn, error)
}

func (a *app) loadDirectMX() error {
	switch os.Getenv("HARBOR_DIRECT_MX") {
	case "false":
		return nil
	case "", "true":
	default:
		return errors.New("HARBOR_DIRECT_MX must be true or false")
	}
	// Some existing installs use a single-label SMTP banner. Keep it valid
	// for inbound mail and use a fully qualified placeholder for direct MX.
	fallback := a.domain
	if !strings.Contains(fallback, ".") || !validDNSName(fallback) || net.ParseIP(fallback) != nil {
		fallback = "harbor.test"
	}
	helo := strings.TrimSuffix(strings.ToLower(env("HARBOR_DIRECT_MX_HELO", fallback)), ".")
	if !strings.Contains(helo, ".") || !validDNSName(helo) || net.ParseIP(helo) != nil {
		return errors.New("HARBOR_DIRECT_MX_HELO must be a fully qualified hostname")
	}
	policy := env("HARBOR_DIRECT_MX_TLS", "required")
	if policy != "required" && policy != "opportunistic" {
		return errors.New("HARBOR_DIRECT_MX_TLS must be required or opportunistic")
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	a.directMX = &directMX{helo, policy == "required", net.DefaultResolver, dialer.DialContext}
	return nil
}

// Disallow public-DNS referrals into the host, private networks, and non-public ranges.
var excludedMXNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

var publicIPv6 = netip.MustParsePrefix("2000::/3")

func publicMXAddress(ip netip.Addr, local []net.Addr) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || (ip.Is6() && !publicIPv6.Contains(ip)) {
		return false
	}
	for _, network := range excludedMXNetworks {
		if network.Contains(ip) {
			return false
		}
	}
	for _, address := range local {
		prefix, err := netip.ParsePrefix(address.String())
		if err == nil && prefix.Addr().Unmap() == ip {
			return false
		}
	}
	return true
}

func dnsNotFound(err error) bool {
	var dnsError *net.DNSError
	return errors.As(err, &dnsError) && dnsError.IsNotFound
}

func (route *directMX) connect(ctx context.Context, recipient string) (outboundSMTP, func(), error) {
	// Absolute names prevent resolver search paths from rewriting public MX lookups.
	domain := emailDomain(recipient)
	if !strings.Contains(domain, ".") || !validDNSName(domain) {
		return outboundSMTP{}, nil, &smtp.SMTPError{Code: 550, Message: "Invalid recipient domain"}
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	established := false
	defer func() {
		if !established {
			cancel()
		}
	}()
	mx, err := route.resolver.LookupMX(ctx, domain+".")
	if err != nil && !dnsNotFound(err) {
		return outboundSMTP{}, nil, fmt.Errorf("MX lookup: %w", err)
	}
	implicit := len(mx) == 0
	if implicit {
		mx = []*net.MX{{Host: domain + "."}}
	}
	if len(mx) == 1 && mx[0].Host == "." && mx[0].Pref == 0 {
		return outboundSMTP{}, nil, &smtp.SMTPError{Code: 556, Message: "Recipient domain does not accept mail"}
	}
	local, err := net.InterfaceAddrs()
	if err != nil {
		return outboundSMTP{}, nil, err
	}
	// Randomize equal-priority servers and preserve the DNS MX preference order.
	rand.Shuffle(len(mx), func(i, j int) { mx[i], mx[j] = mx[j], mx[i] })
	slices.SortStableFunc(mx, func(a, b *net.MX) int { return int(a.Pref) - int(b.Pref) })
	ownPreference := 1 << 16
	for _, record := range mx {
		if strings.EqualFold(strings.TrimSuffix(record.Host, "."), route.helo) {
			ownPreference = int(record.Pref)
			break
		}
	}
	if int(mx[0].Pref) == ownPreference {
		return outboundSMTP{}, nil, &smtp.SMTPError{Code: 550, Message: "Recipient is not a local mailbox"}
	}
	var lastErr error
	for _, record := range mx {
		if int(record.Pref) >= ownPreference {
			break
		}
		host := strings.TrimSuffix(strings.ToLower(record.Host), ".")
		if !strings.Contains(host, ".") || !validDNSName(host) {
			continue
		}
		ips, err := route.resolver.LookupNetIP(ctx, "ip", host+".")
		if err != nil {
			if implicit && dnsNotFound(err) {
				return outboundSMTP{}, nil, &smtp.SMTPError{Code: 550, Message: "Recipient domain has no mail address"}
			}
			lastErr = err
			continue
		}
		for _, ip := range ips {
			if ctx.Err() != nil {
				return outboundSMTP{}, nil, ctx.Err()
			}
			ip = ip.Unmap()
			if !publicMXAddress(ip, local) {
				continue
			}
			client, closeClient, err := route.open(ctx, host, ip)
			if err == nil {
				established = true
				return client, func() { closeClient(); cancel() }, nil
			}
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no eligible public MX address")
	}
	// Connection and handshake failures can be retried even if one host used a 5xx banner.
	return outboundSMTP{}, nil, fmt.Errorf("MX connection unavailable: %v", lastErr)
}

func runMXCheck(out io.Writer, args ...string) error {
	if len(args) != 1 || !strings.Contains(args[0], ".") || !validDNSName(strings.TrimSuffix(args[0], ".")) {
		return errors.New("usage: harbormail mx-check RECIPIENT_DOMAIN")
	}
	a := app{domain: env("HARBOR_DOMAIN", "harbor.test")}
	if err := a.loadDirectMX(); err != nil {
		return err
	}
	if a.directMX == nil {
		return errors.New("MX direct delivery is disabled")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, closeClient, err := a.directMX.connect(ctx, "postmaster@"+args[0])
	if err != nil {
		if code := smtpResponseCode(err); code != 0 {
			return fmt.Errorf("recipient domain cannot receive mail (SMTP %d)", code)
		}
		return errors.New("could not reach a public MX with the configured SMTP TLS policy")
	}
	defer closeClient()
	_, err = fmt.Fprintln(out, "MX lookup and SMTP handshake passed. No email was sent.")
	return err
}

func (route *directMX) open(ctx context.Context, host string, ip netip.Addr) (outboundSMTP, func(), error) {
	conn, err := route.dial(ctx, "tcp", net.JoinHostPort(ip.String(), "25"))
	if err != nil {
		return outboundSMTP{}, nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	cleanup := func() { stop(); _ = conn.Close() }
	deadline := time.Now().Add(12 * time.Second)
	if err := conn.SetDeadline(deadline); err != nil {
		cleanup()
		return outboundSMTP{}, nil, err
	}
	client, err := stdsmtp.NewClient(conn, host)
	if err == nil {
		err = client.Hello(route.helo)
	}
	if err == nil {
		if tlsAvailable, _ := client.Extension("STARTTLS"); tlsAvailable {
			err = client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		} else if route.requireTLS {
			err = errors.New("MX server does not offer STARTTLS")
		}
	}
	if err == nil {
		if deadline, ok := ctx.Deadline(); ok {
			err = conn.SetDeadline(deadline)
		}
	}
	if err != nil {
		cleanup()
		return outboundSMTP{}, nil, err
	}
	return outboundSMTP{client.Mail, client.Rcpt, client.Data}, cleanup, nil
}
