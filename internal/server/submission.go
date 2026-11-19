package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/mail"
	"strings"
	"time"

	"github.com/emersion/go-message/textproto"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

func (a *app) listenSubmission(runtime *serviceRuntime, addr string, config *tls.Config, implicit bool) error {
	if addr == "" {
		return nil
	}
	if config == nil {
		return errors.New("SMTP submission requires TLS certificate configuration")
	}
	server := a.smtpServer(smtp.BackendFunc(func(conn *smtp.Conn) (smtp.Session, error) {
		return &submission{app: a, conn: conn}, nil
	}), config)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen for SMTP submission: %w", err)
	}
	mode := "STARTTLS"
	if implicit {
		listener = tls.NewListener(listener, server.TLSConfig)
		mode = "implicit TLS"
	}
	log.Printf("SMTP submission (%s) listening on %s", mode, listener.Addr())
	runtime.serveSMTP("SMTP submission", server, listener)
	return nil
}

type submission struct {
	app        *app
	conn       *smtp.Conn
	owner      string
	recipients []webTarget
	failures   int
}

func (s *submission) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *submission) Auth(mechanism string) (sasl.Server, error) {
	if mechanism != sasl.Plain {
		return nil, smtp.ErrAuthUnknownMechanism
	}
	if _, encrypted := s.conn.TLSConnectionState(); !encrypted {
		return nil, &smtp.SMTPError{Code: 538, EnhancedCode: smtp.EnhancedCode{5, 7, 11}, Message: "TLS required"}
	}
	if s.failures >= 3 {
		return nil, &smtp.SMTPError{Code: 454, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Too many attempts on this connection"}
	}
	return sasl.NewPlainServer(func(identity, username, password string) error {
		if identity != "" && !strings.EqualFold(strings.TrimSpace(username), identity) {
			s.failures++
			return smtp.ErrAuthFailed
		}
		owner, err := s.app.authenticateProtocol(s.conn.Conn().RemoteAddr(), username, password, clientSMTP)
		if errors.Is(err, errLoginBusy) {
			return &smtp.SMTPError{Code: 454, EnhancedCode: smtp.EnhancedCode{4, 7, 0}, Message: "Authentication temporarily unavailable"}
		}
		if err != nil {
			s.failures++
			return smtp.ErrAuthFailed
		}
		s.owner = owner
		return nil
	}), nil
}

func (s *submission) Mail(from string, _ *smtp.MailOptions) error {
	s.Reset()
	if err := s.app.rejectNewSMTPMail(); err != nil {
		return err
	}
	if s.owner == "" {
		return &smtp.SMTPError{Code: 530, EnhancedCode: smtp.EnhancedCode{5, 7, 0}, Message: "Authentication required"}
	}
	if !strings.EqualFold(from, s.owner) {
		return &smtp.SMTPError{Code: 553, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "Envelope sender must match the authenticated account"}
	}
	return nil
}

func (s *submission) Rcpt(to string, _ *smtp.RcptOptions) error {
	recipient, err := address(to)
	if err != nil || !strings.EqualFold(recipient, to) {
		return &smtp.SMTPError{Code: 553, EnhancedCode: smtp.EnhancedCode{5, 1, 3}, Message: "Invalid recipient"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	target, err := s.app.destination(ctx, s.app.db, recipient)
	if errors.Is(err, errMailboxRetired) {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "Mailbox unavailable"}
	}
	if err != nil {
		return &smtp.SMTPError{Code: 451, Message: "Temporary mailbox failure"}
	}
	if target.owner == "" && !s.app.hasRelayFor(s.owner) {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "External delivery requires a configured relay"}
	}
	for _, existing := range s.recipients {
		if existing.Address == target.address {
			return nil
		}
	}
	if len(s.recipients) >= maxWebRecipients {
		return &smtp.SMTPError{Code: 452, EnhancedCode: smtp.EnhancedCode{4, 5, 3}, Message: "Too many recipients"}
	}
	s.recipients = append(s.recipients, webTarget{Address: target.address, Owner: target.owner})
	return nil
}

func (s *submission) Data(r io.Reader) error {
	if len(s.recipients) == 0 {
		return &smtp.SMTPError{Code: 503, EnhancedCode: smtp.EnhancedCode{5, 5, 1}, Message: "At least one accepted recipient is required"}
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	raw, err = validateSubmission(raw, s.owner)
	if err != nil {
		return &smtp.SMTPError{Code: 554, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: err.Error()}
	}
	envelope := webEnvelope{Recipients: submissionRecipients(raw, s.recipients), Targets: s.recipients}
	m := interpret(raw, s.owner, envelope.Recipients.indexedAddress())
	m.From = s.owner
	m.recipients = &envelope.Recipients
	if err := s.app.storeSubmission(m, raw, envelope); err != nil {
		if isQuotaExceeded(err) {
			return smtpQuotaExceeded()
		}
		if errors.Is(err, errAccountSuspended) {
			return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 7, 1}, Message: "Sender mailbox is suspended"}
		}
		log.Print("SMTP submission storage: ", err)
		return &smtp.SMTPError{Code: 451, EnhancedCode: smtp.EnhancedCode{4, 3, 0}, Message: "Temporary mailbox failure"}
	}
	return nil
}

func (s *submission) Reset() {
	s.recipients = nil
}

func (s *submission) Logout() error { return nil }

func submissionRecipients(raw []byte, targets []webTarget) webRecipients {
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return webRecipients{}
	}
	visible := make(map[string]bool)
	read := func(field string) string {
		var addresses []string
		list, _ := message.Header.AddressList(field)
		for _, entry := range list {
			if value, err := address(entry.Address); err == nil {
				addresses = append(addresses, value)
				visible[strings.ToLower(value)] = true
			}
		}
		return strings.Join(addresses, ", ")
	}
	recipients := webRecipients{To: read("To"), Cc: read("Cc")}
	var blind []string
	for _, target := range targets {
		if !visible[strings.ToLower(target.Address)] {
			blind = append(blind, target.Address)
		}
	}
	recipients.Bcc = strings.Join(blind, ", ")
	return recipients
}

func (a *app) storeSubmission(m letter, raw []byte, envelope webEnvelope) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return err
	}
	if err := a.requireActiveAccount(ctx, tx, m.From); err != nil {
		return err
	}
	for _, target := range envelope.Targets {
		if err := a.checkDestination(ctx, tx, target.Address, target.Owner); err != nil {
			return err
		}
	}
	if len(envelope.externalAddresses()) != 0 {
		id, err := enqueueLetter(tx, m, raw)
		if err != nil {
			return err
		}
		if err := saveWebEnvelope(ctx, tx, id, envelope); err != nil {
			return err
		}
		return a.commitMailbox(tx, m.From)
	}
	owners, err := a.deliverWebLocals(ctx, tx, m, raw, envelope.Targets, false)
	if err != nil {
		return err
	}
	return a.commitMailbox(tx, owners...)
}

func validateSubmission(raw []byte, owner string) ([]byte, error) {
	body := bufio.NewReader(bytes.NewReader(raw))
	header, err := textproto.ReadHeader(body)
	if err != nil {
		return nil, errors.New("Malformed message headers")
	}
	for _, field := range []string{"From", "Sender", "Resent-From", "Resent-Sender"} {
		values := header.Values(field)
		if len(values) == 0 && field != "From" {
			continue
		}
		if len(values) != 1 {
			return nil, errors.New("Message must have a single authorized sender")
		}
		list, err := mail.ParseAddressList(values[0])
		if err != nil || len(list) != 1 || !strings.EqualFold(list[0].Address, owner) {
			return nil, errors.New("Message sender must match the authenticated account")
		}
	}
	if !header.Has("Bcc") && !header.Has("Resent-Bcc") {
		return raw, nil
	}
	header.Del("Bcc")
	header.Del("Resent-Bcc")
	var sanitized bytes.Buffer
	if err := textproto.WriteHeader(&sanitized, header); err != nil {
		return nil, errors.New("Could not prepare message headers")
	}
	_, _ = sanitized.ReadFrom(body)
	return sanitized.Bytes(), nil
}
