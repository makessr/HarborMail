package smtpadmission

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/emersion/go-smtp"
)

const (
	smtpConnectionLimit     = 256
	smtpSourceConnections   = 12
	smtpSourceConnectRate   = 120
	smtpSourceRCPTFailures  = 40
	smtpSessionRCPTLimit    = 120
	smtpSessionRCPTFailures = 10
	smtpSourceBucketLimit   = 8192
)

var smtpAdmissionBusy = &smtp.SMTPError{
	Code: 421, EnhancedCode: smtp.EnhancedCode{4, 7, 0},
	Message: "SMTP rate limit reached, try again later",
}

type smtpSourceBudget struct {
	active, opened, rcptFailed int
	expires                    time.Time
}

type Guard struct {
	mu      sync.Mutex
	sources map[netip.Prefix]*smtpSourceBudget
	active  int
	sweep   time.Time
}

func smtpPeer(addr net.Addr) netip.Prefix {
	if tcp, ok := addr.(*net.TCPAddr); ok {
		ip := tcp.AddrPort().Addr().WithZone("").Unmap()
		if ip.Is4() {
			return netip.PrefixFrom(ip, 32)
		}
		return netip.PrefixFrom(ip, 64).Masked()
	}
	return netip.Prefix{}
}

func (g *Guard) admit(source netip.Prefix, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active >= smtpConnectionLimit {
		return false
	}
	if !now.Before(g.sweep) {
		for key, budget := range g.sources {
			if budget.active == 0 && !now.Before(budget.expires) {
				delete(g.sources, key)
			}
		}
		g.sweep = now.Add(time.Minute)
	}
	budget := g.sources[source]
	if budget == nil {
		if len(g.sources) >= smtpSourceBucketLimit {
			return false
		}
		if g.sources == nil {
			g.sources = make(map[netip.Prefix]*smtpSourceBudget)
		}
		budget = &smtpSourceBudget{}
		g.sources[source] = budget
	}
	budget.refresh(now)
	if budget.active >= smtpSourceConnections || budget.opened >= smtpSourceConnectRate ||
		budget.rcptFailed >= smtpSourceRCPTFailures {
		return false
	}
	budget.active++
	budget.opened++
	g.active++
	return true
}

func (g *Guard) release(source netip.Prefix) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	g.sources[source].active--
}

func (b *smtpSourceBudget) refresh(now time.Time) {
	if !now.Before(b.expires) {
		b.opened, b.rcptFailed = 0, 0
		b.expires = now.Add(time.Minute)
	}
}

func (g *Guard) sourceRCPTAllowed(source netip.Prefix, now time.Time, failed bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if budget := g.sources[source]; budget != nil {
		budget.refresh(now)
		if failed {
			budget.rcptFailed++
		}
		return budget.rcptFailed < smtpSourceRCPTFailures
	}
	return true
}

type RecipientBudget struct {
	attempted, failed int
	expires           time.Time
}

func (b *RecipientBudget) Check(guard *Guard, conn *smtp.Conn, resolve func() error) error {
	if conn == nil {
		return resolve()
	}
	now := time.Now()
	if !now.Before(b.expires) {
		b.attempted, b.failed = 0, 0
		b.expires = now.Add(time.Minute)
	}
	source := smtpPeer(conn.Conn().RemoteAddr())
	if b.attempted >= smtpSessionRCPTLimit || b.failed >= smtpSessionRCPTFailures ||
		!guard.sourceRCPTAllowed(source, now, false) {
		return smtpAdmissionBusy
	}
	b.attempted++
	err := resolve()
	var response *smtp.SMTPError
	if errors.As(err, &response) && response.Code >= 500 && response.Code < 600 {
		b.failed++
		if !guard.sourceRCPTAllowed(source, time.Now(), true) {
			return smtpAdmissionBusy
		}
	}
	return err
}

type Listener struct {
	net.Listener
	Guard       *Guard
	ImplicitTLS bool
}

func (l *Listener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		source := smtpPeer(conn.RemoteAddr())
		if l.Guard.admit(source, time.Now()) {
			return &smtpAdmittedConn{Conn: conn, release: func() { l.Guard.release(source) }}, nil
		}
		if !l.ImplicitTLS {
			_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
			_, _ = io.WriteString(conn, "421 4.7.0 SMTP rate limit reached, try again later\r\n")
		}
		_ = conn.Close()
	}
}

type smtpAdmittedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *smtpAdmittedConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}
