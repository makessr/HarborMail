package server

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/mail"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/emersion/go-message/charset"
	message "github.com/emersion/go-message/mail"
	"github.com/emersion/go-smtp"
)

const mailLimit = 10 << 20

var addrSpec = regexp.MustCompile(`^[a-zA-Z0-9!#$%&'*+/=?^_` + "`" + `{|}~.-]+@[a-zA-Z0-9.-]+$`)

type app struct {
	db                  *sql.DB
	dbDriver            string
	adminCache          *adminRedisCache
	events              mailboxEvents
	outboxWakeup        taskWakeup
	snoozeWakeup        taskWakeup
	stopping            <-chan struct{}
	spamScanner         *rspamdScanner
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
	formatted     bool
}

func token() string {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		log.Fatal(err)
	}
	return hex.EncodeToString(value)
}

func insert(tx *sql.Tx, owner string, m letter, folder string, raw []byte) error {
	_, err := insertLetter(tx, owner, m, folder, folder, raw)
	return err
}

func insertLetter(tx *sql.Tx, owner string, m letter, folder, origin string, raw []byte) (int64, error) {
	var detail []byte
	if folder != "drafts" {
		detail = encodeMailDetail(raw)
		if m.formatted {
			m.Body, _ = renderRichCompose(m.Body)
		}
	}
	result, err := tx.Exec(`INSERT INTO letters(owner,sender,recipient,subject,body,stamp,folder,origin,seen,star,raw,detail_meta)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, owner, strings.ToValidUTF8(m.wireFrom(), "\uFFFD"), m.To,
		strings.ToValidUTF8(m.Subject, "\uFFFD"), strings.ToValidUTF8(m.Body, "\uFFFD"),
		m.Date, folder, origin, m.Read || folder == "sent", m.Star, raw, detail)
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if m.recipients != nil {
		if err := saveWebRecipients(tx, id, *m.recipients); err != nil {
			return 0, err
		}
	}
	return id, indexThread(tx, owner, id, raw)
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
	s.recipients = append(s.recipients, target)
	return nil
}

func (s *delivery) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	folder := "inbox"
	if scanner := s.app.spamScanner; scanner != nil {
		spam, err := scanner.check(s, raw)
		if err != nil {
			log.Print("Rspamd scan unavailable: ", err)
		} else if spam {
			folder = "junk"
		}
	}
	m := interpret(raw, s.sender, "")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx, err := s.app.db.BeginTx(ctx, nil)
	if err == nil {
		defer tx.Rollback()
		owners := make([]string, 0, len(s.recipients))
		err = lockIMAPUIDs(ctx, tx)
		if err == nil {
			for _, recipient := range s.recipients {
				if err = s.app.checkDestination(ctx, tx, recipient.address, recipient.owner); err != nil {
					break
				}
				m.To = recipient.address
				if err = insertIncoming(ctx, tx, recipient.owner, s.sender, folder, m, raw); err != nil {
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
}

func (s *delivery) Logout() error { return nil }

func (a *app) smtpServer(backend smtp.Backend, config *tls.Config) *smtp.Server {
	s := smtp.NewServer(backend)
	s.Domain = a.domain
	s.MaxMessageBytes = mailLimit
	s.MaxRecipients = 20
	s.ReadTimeout = 30 * time.Second
	s.WriteTimeout = 30 * time.Second
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
	parsed, err := mail.ParseAddress(from)
	if err != nil {
		return "", ""
	}
	email, err = address(parsed.Address)
	if err != nil {
		return "", ""
	}
	return email, parsed.Name
}

func encodeLetter(m letter, domain string, files []uploadedAttachment) ([]byte, error) {
	var raw bytes.Buffer
	var header message.Header
	header.SetAddressList("From", []*mail.Address{{Address: m.wireFrom()}})
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
