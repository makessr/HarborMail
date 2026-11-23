package server

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"mime"
	"net/mail"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/emersion/go-message/charset"
	message "github.com/emersion/go-message/mail"
	"github.com/emersion/go-smtp"

	"harbormail/internal/mailstore"
	"harbormail/internal/smtpadmission"
)

const mailLimit = 10 << 20

var addrSpec = regexp.MustCompile(`^[a-zA-Z0-9!#$%&'*+/=?^_` + "`" + `{|}~.-]+@[a-zA-Z0-9.-]+$`)

type app struct {
	webPush             *webPushService
	db                  *sql.DB
	dbDriver            string
	adminCache          *adminRedisCache
	events              mailboxEvents
	outboxWakeup        taskWakeup
	snoozeWakeup        taskWakeup
	stopping            <-chan struct{}
	spamScanner         *rspamdScanner
	scanPolicy          inboundScanPolicy
	smtpAdmission       smtpadmission.Guard
	publicDefaults      bool
	inbound             inboundCapacity
	submissions         inboundCapacity
	sizes               mailSizeLimits
	spamTrainer         *rspamdTrainer
	sharedMailboxEvents bool
	accounts            map[string][]byte
	admins              map[string]bool
	aliases             map[string]string
	sendAliases         map[string]bool
	domain              string
	secure              bool
	recoveryMode        bool
	pauseOutbox         bool
	relay               domainRelay
	directMX            *directMX
	translator          *emailTranslation
	auditDays           int
	auditLogAt          atomic.Int64
	webLogin            webLoginGuard
	clientLogin         webLoginGuard
	contentIndex        int
	adminMFA            map[string]adminMFA
	hardQuota           bool
	domainRelays        map[string]domainRelay
	domainAdmins        map[string]map[string]bool
	domainQuota         map[string]int64
	dkimCipher          cipher.AEAD
}

type letter struct {
	ID            int64            `json:"id"`
	From          string           `json:"from"`
	To            string           `json:"to"`
	Cc            string           `json:"cc,omitempty"`
	Bcc           string           `json:"bcc,omitempty"`
	Subject       string           `json:"subject"`
	Body          string           `json:"body"`
	Date          int64            `json:"date"`
	Folder        string           `json:"folder"`
	Read          bool             `json:"read"`
	Star          bool             `json:"star"`
	SnoozeUntil   int64            `json:"snoozeUntil,omitempty"`
	Attachments   []attachmentInfo `json:"attachments,omitempty"`
	HasHTML       bool             `json:"hasHTML,omitempty"`
	HasText       bool             `json:"hasText,omitempty"`
	Delivery      *deliveryStatus  `json:"delivery,omitempty"`
	Timeline      []deliveryEvent  `json:"timeline,omitempty"`
	Formatted     bool             `json:"formatted,omitempty"`
	DraftVersion  int64            `json:"draftVersion,omitempty"`
	ReplyAddress  string           `json:"replyAddress,omitempty"`
	SenderAddress string           `json:"senderAddress,omitempty"`
	SenderName    string           `json:"senderName,omitempty"`
	Labels        []mailLabel      `json:"labels,omitempty"`
	reply         replyHeaders
	recipients    *webRecipients
	author        string
	fromName      string
	formatted     bool
}

func token() string {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		log.Fatal(err)
	}
	return hex.EncodeToString(value)
}

func insert(ctx context.Context, tx *sql.Tx, owner string, m letter, folder string, raw []byte) error {
	_, err := insertLetter(ctx, tx, owner, m, folder, folder, raw)
	return err
}

func insertLetter(ctx context.Context, tx *sql.Tx, owner string, m letter, folder, origin string, raw []byte) (int64, error) {
	return insertPreparedLetter(ctx, tx, owner, m, folder, origin, prepareLetterContent(raw, folder != "drafts"))
}

type letterContent struct {
	raw    []byte
	detail []byte
	octets int64
	thread messageIdentity
}

func prepareLetterContent(raw []byte, withDetail bool) letterContent {
	content := letterContent{raw: raw, octets: popOctets(raw), thread: identifyMessage(raw)}
	if withDetail {
		content.detail = encodeMailDetail(raw)
	}
	return content
}

func insertPreparedLetter(ctx context.Context, tx *sql.Tx, owner string, m letter, folder, origin string, content letterContent) (int64, error) {
	detail := content.detail
	if folder == "drafts" {
		detail = nil
	} else if m.formatted {
		m.Body, _ = renderRichCompose(m.Body)
	}
	id, err := mailstore.InsertLetter(ctx, tx, mailstore.LetterRow{
		Owner: owner, Sender: strings.ToValidUTF8(m.wireFrom(), "\uFFFD"), Recipient: m.To,
		Subject: strings.ToValidUTF8(m.Subject, "\uFFFD"), Body: strings.ToValidUTF8(m.Body, "\uFFFD"),
		Stamp: m.Date, Folder: folder, Origin: origin, Seen: m.Read || folder == "sent", Star: m.Star,
		Raw: content.raw, Detail: detail, POPOctets: content.octets,
	})
	if err != nil {
		return 0, err
	}
	if m.recipients != nil {
		if err := saveWebRecipients(ctx, tx, id, *m.recipients); err != nil {
			return 0, err
		}
	}
	return id, storeThreadIdentity(ctx, tx, owner, id, content.thread)
}

func interpret(raw []byte, sender, recipient string) letter {
	m := letter{From: sender, To: recipient, Date: time.Now().Unix()}
	reader, err := message.CreateReader(bytes.NewReader(raw))
	if err != nil && reader == nil {
		m.Body = "无法解析邮件正文。可下载原始邮件查看。"
		return m
	}
	defer reader.Close()
	if from, _ := reader.Header.AddressList("From"); len(from) > 0 {
		m.From = from[0].String()
		if from[0].Name == "" {
			m.From = from[0].Address
		}
	}
	m.Subject, _ = reader.Header.Subject()
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		if header, ok := part.Header.(*message.InlineHeader); ok {
			media, _, _ := header.ContentType()
			if media == "text/plain" {
				body, _ := io.ReadAll(io.LimitReader(part.Body, 256<<10))
				m.Body = string(body)
				break
			}
		}
	}
	if m.Body == "" {
		m.Body = "此邮件没有纯文本正文。可下载原始邮件查看 HTML。"
	}
	return m
}

func (a *app) NewSession(conn *smtp.Conn) (smtp.Session, error) {
	return &delivery{app: a, conn: conn}, nil
}

type delivery struct {
	app        *app
	conn       *smtp.Conn
	sender     string
	size       int64
	recipients []mailDestination
	identities map[string][sha256.Size]byte
	rcptBudget smtpadmission.RecipientBudget
}

func (s *delivery) Mail(from string, options *smtp.MailOptions) error {
	s.Reset()
	if err := s.app.rejectNewSMTPMail(); err != nil {
		return err
	}
	s.sender = from
	if options != nil {
		s.size = options.Size
	}
	return nil
}

func (s *delivery) Rcpt(to string, _ *smtp.RcptOptions) error {
	return s.rcptBudget.Check(&s.app.smtpAdmission, s.conn, func() error { return s.resolveRecipient(to) })
}

func (s *delivery) resolveRecipient(to string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	target, err := s.app.destination(ctx, s.app.db, to)
	if errors.Is(err, errMailboxRetired) {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "Mailbox unavailable"}
	}
	if err != nil {
		return &smtp.SMTPError{Code: 451, Message: "Temporary mailbox failure"}
	}
	if target.owner == "" {
		return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "Mailbox unavailable"}
	}
	copies := int64(1)
	for _, existing := range s.recipients {
		if existing.address == target.address {
			return nil
		}
		if existing.owner == target.owner {
			copies++
		}
	}
	if s.app.hardQuota {
		if err := s.app.checkIncomingQuota(ctx, target.owner, s.size, copies); err != nil {
			return err
		}
	}
	if _, bound := s.identities[target.owner]; !bound {
		identity, err := s.app.baseAccountDigest(ctx, s.app.db, target.owner)
		if err != nil {
			return &smtp.SMTPError{Code: 451, Message: "Temporary mailbox failure"}
		}
		if s.identities == nil {
			s.identities = make(map[string][sha256.Size]byte)
		}
		s.identities[target.owner] = identity
	}
	s.recipients = append(s.recipients, target)
	return nil
}

func (s *delivery) Data(r io.Reader) error {
	maxBytes := s.app.messageLimit()
	// Reserve the full limit even when the sender advertises a smaller SIZE.
	if !s.app.inbound.beginMessage(maxBytes) {
		return errInboundCapacity
	}
	defer s.app.inbound.endMessage(maxBytes)
	raw, err := io.ReadAll(io.LimitReader(r, int64(maxBytes)+1))
	if err != nil {
		return err
	}
	if len(raw) > maxBytes {
		return smtp.ErrDataTooLarge
	}
	folder, err := s.scanIncoming(raw)
	if err != nil || folder == "" {
		return err
	}
	m := interpret(raw, s.sender, "")
	content := prepareLetterContent(raw, true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := s.app.db.BeginTx(ctx, nil)
	if err == nil {
		defer tx.Rollback()
		owners := make([]string, 0, len(s.recipients))
		err = lockIMAPUIDs(ctx, tx)
		if err == nil {
			for _, recipient := range s.recipients {
				if err = s.checkRecipient(ctx, tx, recipient); err != nil {
					break
				}
				m.To = recipient.address
				if err = insertPreparedIncoming(ctx, tx, recipient.owner, s.sender, folder, m, content); err != nil {
					break
				}
				owners = append(owners, recipient.owner)
			}
		}
		if err == nil {
			err = s.app.commitMailbox(tx, owners...)
		}
	}
	if err != nil {
		if isQuotaExceeded(err) {
			// DATA has one reply for all accepted recipients, so allow retry.
			return smtpMailboxFull()
		}
		log.Print("incoming mail storage: ", err)
		return &smtp.SMTPError{Code: 451, Message: "Temporary mailbox failure"}
	}
	return nil
}

func (s *delivery) Reset() {
	s.sender = ""
	s.size = 0
	s.recipients = nil
	s.identities = nil
}

func (s *delivery) checkRecipient(ctx context.Context, tx *sql.Tx, recipient mailDestination) error {
	if err := s.app.checkDestination(ctx, tx, recipient.address, recipient.owner); err != nil {
		return err
	}
	if before, bound := s.identities[recipient.owner]; bound {
		identity, err := s.app.baseAccountDigest(ctx, tx, recipient.owner)
		if err != nil {
			return err
		}
		if identity != before {
			return errDestinationChanged
		}
	}
	return nil
}

func (s *delivery) Logout() error { return nil }

func (a *app) smtpServer(backend smtp.Backend, config *tls.Config) *smtp.Server {
	s := smtp.NewServer(backend)
	s.Domain = a.domain
	s.MaxMessageBytes = int64(a.messageLimit())
	s.MaxRecipients = 20
	s.ReadTimeout = 30 * time.Second
	s.WriteTimeout = 30 * time.Second
	if a.messageLimit() > mailLimit {
		s.ReadTimeout = 2 * time.Minute
	}
	if config != nil {
		s.TLSConfig = config.Clone()
	}
	return s
}

func address(value string) (string, error) {
	m, err := mail.ParseAddress(strings.TrimSpace(value))
	if err != nil || !addrSpec.MatchString(m.Address) || len(m.Address) > 254 || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("invalid email address")
	}
	local, domain, _ := strings.Cut(m.Address, "@")
	return local + "@" + strings.ToLower(domain), nil
}

func senderContact(from string) (email, name string) {
	parsed, err := message.ParseAddress(from)
	if err != nil {
		return "", ""
	}
	email, err = address(parsed.Address)
	if err != nil {
		return "", ""
	}
	// The address parser leaves encoded words inside quoted display names intact.
	name = parsed.Name
	if strings.Contains(name, "=?") {
		decoder := mime.WordDecoder{CharsetReader: charset.Reader}
		if decoded, err := decoder.DecodeHeader(name); err == nil && !strings.ContainsFunc(decoded, unicode.IsControl) {
			name = decoded
		}
	}
	return email, name
}

func displaySender(from string) string {
	email, name := senderContact(from)
	if name != "" {
		return name + " <" + email + ">"
	}
	if email != "" {
		return email
	}
	return from
}

func encodeLetter(m letter, domain string, files []uploadedAttachment) ([]byte, error) {
	var raw bytes.Buffer
	var header message.Header
	header.SetAddressList("From", []*mail.Address{{Name: m.fromName, Address: m.wireFrom()}})
	if m.author != "" && m.author != m.From {
		header.Set("X-Harbor-Send-As", m.author)
	}
	if m.formatted {
		header.Set("X-Harbor-Format", "rich")
	}
	if m.recipients != nil {
		setVisibleRecipients(&header, *m.recipients)
	} else if m.To != "" {
		header.SetAddressList("To", []*mail.Address{{Address: m.To}})
	}
	header.SetSubject(m.Subject)
	header.SetDate(time.Unix(m.Date, 0))
	header.SetMessageID(token() + "@" + domain)
	if len(m.reply.parents) > 0 {
		header.SetMsgIDList("In-Reply-To", m.reply.parents)
	}
	if len(m.reply.references) > 0 {
		header.SetMsgIDList("References", m.reply.references)
	}
	header.Set("Content-Type", "text/plain; charset=UTF-8")
	if len(files) == 0 {
		if m.formatted {
			inline, err := message.CreateInlineWriter(&raw, header)
			if err != nil {
				return nil, err
			}
			err = writeRichCompose(inline, m.Body)
			return raw.Bytes(), err
		}
		part, err := message.CreateSingleInlineWriter(&raw, header)
		if err != nil {
			return nil, err
		}
		err = writeMIMEPart(part, []byte(m.Body))
		return raw.Bytes(), err
	}
	writer, err := message.CreateWriter(&raw, header)
	if err != nil {
		return nil, err
	}
	if m.formatted {
		inline, err := writer.CreateInline()
		if err != nil {
			return nil, err
		}
		if err := writeRichCompose(inline, m.Body); err != nil {
			return nil, err
		}
	} else {
		var text message.InlineHeader
		text.Set("Content-Type", "text/plain; charset=UTF-8")
		part, err := writer.CreateSingleInline(text)
		if err != nil {
			return nil, err
		}
		if err := writeMIMEPart(part, []byte(m.Body)); err != nil {
			return nil, err
		}
	}
	for _, file := range files {
		var header message.AttachmentHeader
		header.Set("Content-Type", "application/octet-stream")
		header.SetFilename(file.name)
		part, err := writer.CreateAttachment(header)
		if err != nil {
			return nil, err
		}
		if err := writeMIMEPart(part, file.data); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return raw.Bytes(), nil
}

func writeMIMEPart(part io.WriteCloser, data []byte) error {
	_, err := part.Write(data)
	return errors.Join(err, part.Close())
}
