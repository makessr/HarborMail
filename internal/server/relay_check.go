package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/emersion/go-smtp"
)

func runRelayCheck(out io.Writer, sender string) error {
	sender = strings.TrimSpace(sender)
	email, err := address(sender)
	if err != nil || !strings.EqualFold(email, sender) {
		return errors.New("relay-check requires a plain sender email address")
	}
	var a app
	if err := a.loadRelays(); err != nil {
		return err
	}
	relay := a.sendRelay(email)
	if relay.Address == "" {
		return errors.New("no SMTP relay is configured for the sender domain")
	}
	scope := "global"
	if _, exists := a.domainRelays[emailDomain(email)]; exists {
		scope = "per-domain"
	}
	if _, err := fmt.Fprintf(out, "SMTP relay %q (%s, %s)\n", relay.Address, scope, relay.TLS); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, closeClient, err := a.connectSenderRelay(ctx, email)
	if err != nil {
		return relayCheckError(ctx, err)
	}
	defer closeClient()
	// The SMTP client completes a STARTTLS handshake at the first encrypted command.
	if err := client.Noop(); err != nil {
		return relayCheckError(ctx, err)
	}
	if relay.User != "" {
		_, err = fmt.Fprintln(out, "TLS and SMTP authentication verified")
	} else {
		_, err = fmt.Fprintln(out, "TLS verified; SMTP authentication is not configured")
	}
	return err
}

func relayCheckError(ctx context.Context, err error) error {
	// Only print fixed messages and SMTP reply codes. Remote text can contain credentials.
	var certErr *tls.CertificateVerificationError
	var networkErr *net.OpError
	var timeout net.Error
	var reply *smtp.SMTPError
	switch {
	case ctx.Err() != nil, errors.As(err, &timeout) && timeout.Timeout():
		return errors.New("SMTP relay check timed out")
	case errors.As(err, &certErr):
		return errors.New("SMTP relay certificate verification failed; check the hostname and CA")
	case errors.Is(err, errRelayAuthentication):
		if errors.As(err, &reply) {
			return fmt.Errorf("SMTP relay authentication failed (SMTP %d)", reply.Code)
		}
		return errRelayAuthentication
	case errors.As(err, &networkErr) && networkErr.Op == "dial":
		return errors.New("cannot connect to SMTP relay; check DNS, port and network access")
	case errors.As(err, &reply):
		return fmt.Errorf("SMTP relay check failed (SMTP %d)", reply.Code)
	default:
		return errors.New("SMTP relay did not complete the TLS and SMTP check; verify the TLS mode")
	}
}
