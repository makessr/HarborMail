package smtpadmission

import (
	"net/netip"
	"testing"
	"time"
)

func TestSMTPAdmissionPerSourceAndPreDATA(t *testing.T) {
	guard := &Guard{}
	first := netip.MustParsePrefix("192.0.2.20/32")
	second := netip.MustParsePrefix("192.0.2.21/32")
	now := time.Now()
	for range smtpSourceConnections {
		if !guard.admit(first, now) {
			t.Fatal("per-source limit reached early")
		}
	}
	if guard.admit(first, now) || !guard.admit(second, now) {
		t.Fatal("connection cap did not isolate sources")
	}
	for range smtpSourceConnections {
		guard.release(first)
	}
	for i := smtpSourceConnections; i < smtpSourceConnectRate; i++ {
		if !guard.admit(first, now) {
			t.Fatal("source rate reached early")
		}
		guard.release(first)
	}
	if guard.admit(first, now) || !guard.admit(first, now.Add(time.Minute)) {
		t.Fatal("source connection rate did not reset")
	}
	now = now.Add(time.Minute)
	for range smtpSourceRCPTFailures {
		guard.sourceRCPTAllowed(first, now, true)
	}
	if guard.sourceRCPTAllowed(first, now, false) || !guard.sourceRCPTAllowed(second, now, false) {
		t.Fatal("recipient failure limit did not isolate sources")
	}
}
