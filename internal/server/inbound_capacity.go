package server

import (
	"fmt"
	"strconv"
	"sync"

	"github.com/emersion/go-smtp"
)

type inboundLimits struct {
	messages, bytes, scans int
}

var defaultInboundLimits = inboundLimits{messages: 4, bytes: 64 << 20, scans: 2}

func loadInboundLimits(messageLimit int) (inboundLimits, error) {
	var limits inboundLimits
	for _, setting := range []struct {
		name          string
		dest          *int
		fallback, max int
	}{
		{"HARBOR_INBOUND_MAX_INFLIGHT", &limits.messages, defaultInboundLimits.messages, 64},
		{"HARBOR_INBOUND_MAX_MIB", &limits.bytes, defaultInboundLimits.bytes >> 20, 1024},
		{"HARBOR_INBOUND_SCAN_CONCURRENCY", &limits.scans, defaultInboundLimits.scans, 64},
	} {
		value, err := strconv.Atoi(env(setting.name, strconv.Itoa(setting.fallback)))
		if err != nil || value < 1 || value > setting.max {
			return limits, fmt.Errorf("%s must be an integer from 1 to %d", setting.name, setting.max)
		}
		*setting.dest = value
	}
	limits.bytes <<= 20
	if limits.bytes < messageLimit {
		return limits, fmt.Errorf("HARBOR_INBOUND_MAX_MIB must be at least HARBOR_MAX_MESSAGE_MIB")
	}
	return limits, nil
}

type inboundUsage struct {
	messages, bytes, scans int
	deferred               uint64
}

type inboundCapacity struct {
	mu     sync.Mutex
	limits inboundLimits
	usage  inboundUsage
}

var errInboundCapacity = &smtp.SMTPError{
	Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 2}, Message: "Inbound processing temporarily at capacity; try again later",
}

func (c *inboundCapacity) configuredLimits() inboundLimits {
	if c.limits.messages == 0 {
		return defaultInboundLimits
	}
	return c.limits
}

func (c *inboundCapacity) beginMessage(bytes int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	limits := c.configuredLimits()
	if c.usage.messages >= limits.messages || bytes > limits.bytes-c.usage.bytes {
		c.usage.deferred++
		return false
	}
	c.usage.messages++
	c.usage.bytes += bytes
	return true
}

func (c *inboundCapacity) endMessage(bytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.usage.messages--
	c.usage.bytes -= bytes
}

func (c *inboundCapacity) beginScan(mailDelivery bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.usage.scans >= c.configuredLimits().scans {
		if mailDelivery {
			c.usage.deferred++
		}
		return false
	}
	c.usage.scans++
	return true
}

func (c *inboundCapacity) endScan() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.usage.scans--
}

func (c *inboundCapacity) snapshot() inboundUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usage
}
