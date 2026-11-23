package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	loginWindow      = 5 * time.Minute
	loginSourceLimit = 30
	loginPairLimit   = 8
	loginBucketLimit = 8192
)

type loginBucketKey struct {
	source     netip.Prefix
	login      string
	perAccount bool
}

type loginBucket struct {
	expires  time.Time
	attempts int
}

type webLoginGuard struct {
	shared    *mysqlLoginLimiter
	scope     string
	proxies   []netip.Prefix
	mutex     sync.Mutex
	buckets   map[loginBucketKey]loginBucket
	nextSweep time.Time
}

func (g *webLoginGuard) reserve(ctx context.Context, source netip.Prefix, email string, perAccount bool) (time.Duration, error) {
	if g.shared != nil {
		return g.shared.reserve(ctx, g.scope, loginBucketKey{source: source, login: email, perAccount: perAccount})
	}
	return g.allow(time.Now(), source, email, perAccount), nil
}

func parseTrustedProxies(value string) ([]netip.Prefix, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var rules []netip.Prefix
	for _, token := range strings.Split(value, ",") {
		token = strings.TrimSpace(token)
		prefix, err := netip.ParsePrefix(token)
		if err != nil {
			address, addrErr := netip.ParseAddr(token)
			if addrErr != nil || address.Zone() != "" || address.IsUnspecified() {
				return nil, errors.New("HARBOR_TRUSTED_PROXIES must contain trusted IP addresses or CIDRs")
			}
			address = address.Unmap()
			prefix = netip.PrefixFrom(address, address.BitLen())
		}
		if prefix.Bits() == 0 || prefix.Addr().Is4In6() || prefix.Addr().IsMulticast() {
			return nil, errors.New("HARBOR_TRUSTED_PROXIES must contain specific unicast IP addresses or CIDRs")
		}
		rules = append(rules, prefix.Masked())
	}
	return rules, nil
}

func loginSource(r *http.Request, proxies []netip.Prefix) netip.Prefix {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, _ := netip.ParseAddr(host)
	peer = peer.WithZone("").Unmap()
	trusted := func(ip netip.Addr) bool {
		return slices.ContainsFunc(proxies, func(prefix netip.Prefix) bool { return prefix.Contains(ip) })
	}
	selected := peer
	if trusted(peer) {
		header := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
		if len(header) <= 4096 && header != "" {
			hops := strings.Split(header, ",")
			for i := len(hops) - 1; i >= 0; i-- {
				hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
				if err != nil || hop.Zone() != "" || hop.IsUnspecified() || hop.IsMulticast() {
					break
				}
				hop = hop.Unmap()
				if !trusted(hop) {
					selected = hop
					break
				}
			}
		}
	}
	if selected.Is4() {
		return netip.PrefixFrom(selected, 32)
	}
	return netip.PrefixFrom(selected, 64).Masked()
}

func (g *webLoginGuard) allow(now time.Time, source netip.Prefix, email string, perAccount bool) time.Duration {
	limit := loginSourceLimit
	if perAccount {
		limit = loginPairLimit
	}
	key := loginBucketKey{source: source, login: email, perAccount: perAccount}
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if g.buckets == nil {
		g.buckets = make(map[loginBucketKey]loginBucket)
	}
	if !now.Before(g.nextSweep) {
		for key, bucket := range g.buckets {
			if !now.Before(bucket.expires) {
				delete(g.buckets, key)
			}
		}
		g.nextSweep = now.Add(time.Minute)
	}
	bucket, exists := g.buckets[key]
	if !now.Before(bucket.expires) {
		if !exists && len(g.buckets) >= loginBucketLimit {
			return time.Minute
		}
		bucket = loginBucket{expires: now.Add(loginWindow)}
	}
	if bucket.attempts >= limit {
		return bucket.expires.Sub(now)
	}
	bucket.attempts++
	g.buckets[key] = bucket
	return 0
}

func (a *app) rejectWebLoginRate(w http.ResponseWriter, source netip.Prefix, account string, retry time.Duration) {
	a.recordLogin(source, account, "blocked", "web")
	seconds := int((retry + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	fail(w, http.StatusTooManyRequests, fmt.Sprintf("登录尝试过多，请在 %d 秒后重试", seconds))
}

func (a *app) checkWebLoginRate(w http.ResponseWriter, ctx context.Context, source netip.Prefix, account string, perAccount bool) bool {
	retry, err := a.webLogin.reserve(ctx, source, account, perAccount)
	if err != nil {
		a.recordLogin(source, loginAuditAccount(account), "error", "web")
		loginUnavailable(w)
		return false
	}
	if retry > 0 {
		a.rejectWebLoginRate(w, source, loginAuditAccount(account), retry)
		return false
	}
	return true
}

func loginUnavailable(w http.ResponseWriter) {
	fail(w, http.StatusServiceUnavailable, "暂时无法验证登录，请稍后重试")
}

func loginTemporaryError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, errLoginUnavailable):
		loginUnavailable(w)
	case errors.Is(err, errLoginBusy):
		w.Header().Set("Retry-After", "1")
		fail(w, http.StatusTooManyRequests, "验证请求过多，请稍后重试")
	default:
		return false
	}
	return true
}
