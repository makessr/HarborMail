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
	loginRecentLimit = 100
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

type loginEvent struct {
	At       int64  `json:"at"`
	Source   string `json:"source"`
	Account  string `json:"account,omitempty"`
	Result   string `json:"result"`
	Protocol string `json:"protocol"`
}

type webLoginGuard struct {
	shared    *mysqlLoginLimiter
	scope     string
	proxies   []netip.Prefix
	mutex     sync.Mutex
	buckets   map[loginBucketKey]loginBucket
	nextSweep time.Time
	recent    [loginRecentLimit]loginEvent
	next      int
	size      int
	totals    map[string]uint64
}

func (g *webLoginGuard) reserve(ctx context.Context, source netip.Prefix, email string, perAccount bool) time.Duration {
	if g.shared != nil {
		return g.shared.reserve(ctx, g.scope, loginBucketKey{source: source, login: email, perAccount: perAccount})
	}
	return g.allow(time.Now(), source, email, perAccount)
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

func (g *webLoginGuard) record(source netip.Prefix, account, result string) {
	g.recordProtocol(source, account, result, "web")
}

func (g *webLoginGuard) recordProtocol(source netip.Prefix, account, result, protocol string) {
	value := "未知"
	if source.IsValid() {
		value = source.String()
		if source.Addr().Is4() {
			value = source.Addr().String()
		}
	}
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if g.totals == nil {
		g.totals = make(map[string]uint64)
	}
	g.totals[result]++
	g.recent[g.next] = loginEvent{At: time.Now().Unix(), Source: value, Account: account, Result: result, Protocol: protocol}
	g.next = (g.next + 1) % len(g.recent)
	g.size = min(g.size+1, len(g.recent))
}

func (a *app) rejectWebLoginRate(w http.ResponseWriter, source netip.Prefix, account string, retry time.Duration) {
	a.webLogin.record(source, account, "blocked")
	seconds := int((retry + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	fail(w, http.StatusTooManyRequests, fmt.Sprintf("登录尝试过多，请在 %d 秒后重试", seconds))
}

func (a *app) adminSecurity(w http.ResponseWriter, _ *http.Request, actor string) {
	g := &a.webLogin
	g.mutex.Lock()
	events := make([]loginEvent, 0, g.size)
	for i := 0; i < g.size; i++ {
		index := (g.next - 1 - i + len(g.recent)) % len(g.recent)
		events = append(events, g.recent[index])
	}
	totals := make(map[string]uint64, len(g.totals))
	for key, count := range g.totals {
		totals[key] = count
	}
	g.mutex.Unlock()
	reply(w, http.StatusOK, map[string]any{
		"email": actor, "events": events, "totals": totals, "windowSeconds": int(loginWindow.Seconds()),
		"sourceLimit": loginSourceLimit, "pairLimit": loginPairLimit, "trustedProxies": len(g.proxies),
		"sharedLimit": g.shared != nil,
	})
}
