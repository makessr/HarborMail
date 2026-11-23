package server

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

var errDKIMSigning = errors.New("DKIM signing unavailable")
var errDKIMAuthor = errors.New("DKIM requires one From address matching the envelope sender")

func loadDKIMCipher() (cipher.AEAD, error) {
	value, err := fileSetting("HARBOR_DKIM_KEY")
	if err != nil {
		return nil, err
	}
	if value == "" && os.Getenv("HARBOR_DKIM_KEY_FILE") != "" {
		return nil, errors.New("the configured DKIM key file is empty")
	}
	if value == "" {
		if path := os.Getenv("HARBOR_DKIM_LOCAL_KEY_FILE"); path != "" {
			stored, err := os.ReadFile(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			if err == nil && strings.TrimSpace(string(stored)) == "" {
				return nil, errors.New("the local DKIM key file is empty")
			}
			value = string(stored)
		}
	}
	if value == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil || len(key) != 32 {
		return nil, errors.New("HARBOR_DKIM_KEY must contain 32 bytes encoded as 64 hex characters")
	}
	block, err := aes.NewCipher(key)
	clear(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

// The local wrapping key travels with the server's persistent data, separate from MySQL.
func (a *app) loadServerDKIM() (cipher.AEAD, error) {
	box, err := loadDKIMCipher()
	path := os.Getenv("HARBOR_DKIM_LOCAL_KEY_FILE")
	if err != nil || box != nil || path == "" || a.recoveryMode {
		return box, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var exists bool
	if err := a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM domain_dkim_keys) OR EXISTS(SELECT 1 FROM mailbox_mfa)").Scan(&exists); err != nil {
		return nil, err
	}
	if exists {
		return loadDKIMCipher()
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, err
	}
	defer clear(key[:])
	f, err := os.CreateTemp(filepath.Dir(path), ".dkim-key-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	var encoded [64]byte
	hex.Encode(encoded[:], key[:])
	defer clear(encoded[:])
	_, writeErr := f.Write(encoded[:])
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return nil, err
	}
	// Publish only a complete key without replacing a key created by another instance.
	if err := os.Link(f.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	return loadDKIMCipher()
}

type dkimPublicKey struct {
	Selector string `json:"selector"`
	Name     string `json:"name"`
	TXT      string `json:"txt"`
}

func dkimDNSKey(domain, selector, public string) dkimPublicKey {
	return dkimPublicKey{selector, selector + "._domainkey." + domain, "v=DKIM1; k=rsa; p=" + public}
}

func generateDKIMKey(box cipher.AEAD, domain string) (selector, public string, sealed []byte, err error) {
	var id [8]byte
	if _, err = rand.Read(id[:]); err != nil {
		return
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", nil, err
	}
	selector = "harbor-" + hex.EncodeToString(id[:])
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", nil, err
	}
	privateDER := x509.MarshalPKCS1PrivateKey(key)
	defer clear(privateDER)
	return selector, base64.StdEncoding.EncodeToString(publicDER), box.Seal(nil, nil, privateDER, []byte(domain+"\x00"+selector)), nil
}

func openDKIMKey(box cipher.AEAD, domain, selector string, sealed []byte) (*rsa.PrivateKey, error) {
	if box == nil {
		return nil, errDKIMSigning
	}
	data, err := box.Open(nil, nil, sealed, []byte(domain+"\x00"+selector))
	if err != nil {
		return nil, errDKIMSigning
	}
	defer clear(data)
	key, err := x509.ParsePKCS1PrivateKey(data)
	if err != nil {
		return nil, errDKIMSigning
	}
	return key, nil
}

func checkDKIMPublicKey(ctx context.Context, resolver *net.Resolver, name, public string) string {
	records, err := resolver.LookupTXT(ctx, name+".")
	if state := classifyDNS(len(records), err).State; state != "present" {
		return state
	}
	if len(records) != 1 {
		return "multiple"
	}
	tags := make(map[string]string)
	for _, field := range strings.Split(records[0], ";") {
		key, value, ok := strings.Cut(field, "=")
		key = strings.TrimSpace(key)
		if _, duplicate := tags[key]; duplicate {
			return "mismatch"
		}
		if ok {
			tags[key] = strings.Join(strings.Fields(value), "")
		}
	}
	if len(tags) == 3 && tags["v"] == "DKIM1" && tags["k"] == "rsa" && tags["p"] == public {
		return "match"
	}
	return "mismatch"
}

func activeDKIMSelector(ctx context.Context, db folderQueryer, domain string) (string, error) {
	var selector string
	err := db.QueryRowContext(ctx, `SELECT selector FROM domain_dkim_keys WHERE domain=? AND slot='active'`, domain).Scan(&selector)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return selector, err
}

func signDKIMHeader(raw []byte, sender, domain, selector string, key *rsa.PrivateKey) (string, error) {
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil || len(message.Header["From"]) != 1 {
		return "", errDKIMAuthor
	}
	author, err := address(message.Header.Get("From"))
	if err != nil || author != sender {
		return "", errDKIMAuthor
	}
	signer, err := dkim.NewSigner(&dkim.SignOptions{
		Domain: domain, Selector: selector, Signer: key,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationSimple,
		HeaderKeys: []string{"From", "From", "To", "Cc", "Subject", "Date", "Message-ID", "Reply-To",
			"In-Reply-To", "References", "MIME-Version", "Content-Type", "Content-Transfer-Encoding"},
	})
	if err != nil {
		return "", errDKIMSigning
	}
	_, writeErr := signer.Write(raw)
	if err := signer.Close(); err != nil || writeErr != nil {
		return "", errDKIMSigning
	}
	return signer.Signature(), nil
}

func (a *app) prepareDKIM(ctx context.Context, job *outboxJob) error {
	if !job.dkimPrepared {
		job.dkimHeader, job.dkimErr = a.outboundDKIMHeader(ctx, job)
		job.dkimPrepared = true
	}
	return job.dkimErr
}

func (a *app) outboundDKIMHeader(ctx context.Context, job *outboxJob) (string, error) {
	domain := emailDomain(job.message.wireFrom())
	var selector string
	var sealed []byte
	var cached sql.NullString
	err := a.db.QueryRowContext(ctx, `SELECT k.selector,k.sealed_key,s.header FROM domain_dkim_keys k
		LEFT JOIN outbox_dkim_signatures s ON s.outbox_id=? AND s.selector=k.selector
		WHERE k.domain=? AND k.slot='active'`, job.message.ID, domain).Scan(&selector, &sealed, &cached)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil || a.dkimCipher == nil {
		return "", errDKIMSigning
	}
	if cached.Valid {
		return cached.String, nil
	}
	key, err := openDKIMKey(a.dkimCipher, domain, selector, sealed)
	if err != nil {
		return "", err
	}
	header, err := signDKIMHeader(job.raw, job.message.wireFrom(), domain, selector, key)
	if err != nil {
		return "", err
	}
	upsert := ` ON CONFLICT(outbox_id) DO UPDATE SET selector=excluded.selector,header=excluded.header`
	if a.dbDriver == "mysql" {
		upsert = ` ON DUPLICATE KEY UPDATE selector=VALUES(selector),header=VALUES(header)`
	}
	result, err := a.db.ExecContext(ctx, `INSERT INTO outbox_dkim_signatures(outbox_id,selector,header)
		SELECT id,?,? FROM outbox WHERE id=? AND claim=? AND state='preparing' AND lease>?`+upsert,
		selector, header, job.message.ID, job.claim, time.Now().Unix())
	if err != nil {
		return "", errDKIMSigning
	}
	if count, err := result.RowsAffected(); err != nil || count == 0 {
		return "", errDKIMSigning
	}
	return header, nil
}

func (job *outboxJob) writeExternalMessage(w io.Writer) error {
	if job.dkimHeader != "" {
		if _, err := io.WriteString(w, job.dkimHeader); err != nil {
			return err
		}
	}
	_, err := io.Copy(w, bytes.NewReader(job.raw))
	return err
}
