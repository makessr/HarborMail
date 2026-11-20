package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
)

type dnsCheck struct {
	State string `json:"state"`
	Count int    `json:"count,omitempty"`
}

type domainDNS struct {
	MX           dnsCheck  `json:"mx"`
	Hosts        []*net.MX `json:"hosts,omitempty"`
	SPF          dnsCheck  `json:"spf"`
	DMARC        dnsCheck  `json:"dmarc"`
	DKIM         dnsCheck  `json:"dkim"`
	DKIMSelector string    `json:"dkimSelector,omitempty"`
}

func runDomainDNSCheck(out io.Writer, resolver *net.Resolver, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: harbormail dns-check DOMAIN [DKIM_SELECTOR | --configured]")
	}
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(args[0])), ".")
	if !strings.Contains(domain, ".") || !validDNSName(domain) || len("_dmarc."+domain) > 253 {
		return errors.New("provide a fully qualified mail domain; use Punycode for internationalized domains")
	}
	selector := ""
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if len(args) == 2 {
		selector = strings.ToLower(strings.TrimSpace(args[1]))
		if selector == "--configured" {
			var err error
			selector, err = configuredDKIMSelector(ctx, domain)
			if err != nil {
				return err
			}
		} else if !validDNSName(selector) || len(selector+"._domainkey."+domain) > 253 {
			return errors.New("invalid DKIM selector")
		}
	}

	result := lookupDomainDNS(ctx, resolver, domain, selector)
	report := struct {
		Domain string `json:"domain"`
		domainDNS
	}{domain, result}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return err
	}
	if result.MX.State != "present" || result.SPF.State != "present" || result.DMARC.State != "present" ||
		(selector != "" && result.DKIM.State != "present") {
		return errors.New("mail DNS records need attention; see the states above")
	}
	return nil
}

func configuredDKIMSelector(ctx context.Context, domain string) (string, error) {
	db, err := openConfiguredCheckDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	selector, err := activeDKIMSelector(ctx, db, domain)
	if err != nil {
		return "", errors.New("could not read the active DKIM selector from the configured database")
	}
	if selector == "" {
		return "", errors.New("no active DKIM selector is configured for this domain")
	}
	return selector, nil
}

func openConfiguredCheckDB() (*sql.DB, error) {
	switch env("HARBOR_DB_DRIVER", "sqlite") {
	case "sqlite":
		path, err := filepath.Abs(env("HARBOR_DB", "data/harbor.db"))
		if err != nil {
			return nil, errors.New("could not resolve the configured SQLite database path")
		}
		path = filepath.ToSlash(path)
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
		db, err := sql.Open("sqlite", uri.String())
		if err != nil {
			return nil, errors.New("could not open the configured SQLite database")
		}
		return db, nil
	case "mysql":
		source, err := fileSetting("HARBOR_MYSQL_DSN")
		if err != nil {
			return nil, err
		}
		config, err := harborMySQLConfig(source)
		if err != nil {
			return nil, err
		}
		connector, err := mysql.NewConnector(config)
		if err != nil {
			return nil, errors.New("could not configure the MySQL connection")
		}
		return sql.OpenDB(connector), nil
	default:
		return nil, errors.New("use the sqlite or mysql database driver")
	}
}

func lookupDomainDNS(ctx context.Context, resolver *net.Resolver, domain, selector string) domainDNS {
	dkimName := selector + "._domainkey." + domain
	var result domainDNS
	result.DKIM.State = "skipped"
	result.DKIMSelector = selector
	var checks sync.WaitGroup
	checks.Go(func() {
		hosts, err := resolver.LookupMX(ctx, domain+".")
		result.MX = classifyDNS(len(hosts), err)
		if len(hosts) == 1 && hosts[0].Host == "." && hosts[0].Pref == 0 {
			result.MX.State = "null"
		}
		result.Hosts = hosts
	})
	checks.Go(func() {
		result.SPF = checkTXTDNS(ctx, resolver, domain+".", func(record string) bool {
			fields := strings.Fields(record)
			return len(fields) > 0 && strings.EqualFold(fields[0], "v=spf1")
		})
	})
	checks.Go(func() {
		result.DMARC = checkTXTDNS(ctx, resolver, "_dmarc."+domain+".", func(record string) bool {
			first, _, _ := strings.Cut(record, ";")
			key, value, _ := strings.Cut(first, "=")
			return strings.TrimSpace(key) == "v" && strings.TrimSpace(value) == "DMARC1"
		})
	})
	if selector != "" {
		checks.Go(func() {
			result.DKIM = checkTXTDNS(ctx, resolver, dkimName+".", func(record string) bool {
				for _, field := range strings.Split(record, ";") {
					key, value, _ := strings.Cut(field, "=")
					if strings.TrimSpace(key) == "p" && strings.TrimSpace(value) != "" {
						return true
					}
				}
				return false
			})
		})
	}
	checks.Wait()
	return result
}

func checkTXTDNS(ctx context.Context, resolver *net.Resolver, name string, matches func(string) bool) dnsCheck {
	records, err := resolver.LookupTXT(ctx, name)
	count := 0
	for _, record := range records {
		if matches(record) {
			count++
		}
	}
	result := classifyDNS(count, err)
	if result.State == "present" && count > 1 {
		result.State = "multiple"
	}
	return result
}

func classifyDNS(count int, err error) dnsCheck {
	if err != nil {
		var dnsErr *net.DNSError
		if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
			return dnsCheck{State: "error"}
		}
	}
	if count == 0 {
		return dnsCheck{State: "missing"}
	}
	return dnsCheck{State: "present", Count: count}
}
