package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

type imapImportOptions struct {
	host, user, owner, passwordFile, caFile, root string
}

func parseIMAPImport(args []string) (imapImportOptions, error) {
	var opt imapImportOptions
	flags := flag.NewFlagSet("import-imap", flag.ContinueOnError)
	flags.StringVar(&opt.host, "host", "", "source IMAPS address (host:port)")
	flags.StringVar(&opt.user, "user", "", "source login")
	flags.StringVar(&opt.owner, "owner", "", "destination Harbor Mail primary address")
	flags.StringVar(&opt.passwordFile, "password-file", "", "source password file, or - for standard input")
	flags.StringVar(&opt.caFile, "ca-file", "", "add a PEM CA for the source TLS certificate")
	flags.StringVar(&opt.root, "folder", "", "destination folder prefix")
	if err := flags.Parse(args); err != nil {
		return opt, err
	}
	host, port, err := net.SplitHostPort(opt.host)
	owner, ownerErr := address(opt.owner)
	if err != nil || host == "" || port == "" || ownerErr != nil || owner != opt.owner || owner != strings.ToLower(owner) ||
		flags.NArg() != 0 || opt.user == "" || opt.passwordFile == "" {
		return opt, errors.New("required: --host HOST:PORT --user LOGIN --owner PRIMARY_EMAIL --password-file PATH")
	}
	if opt.root == "" {
		opt.root = "迁移-" + imapImportKey(opt.host, opt.user)[:8]
	}
	var invalidRoot error
	opt.root, invalidRoot = folderName(opt.root)
	if invalidRoot != nil || utf8.RuneCountInString(opt.root) > 40 {
		return opt, errors.New("--folder must be a valid Harbor Mail folder path of at most 40 characters")
	}
	return opt, nil
}

func imapImportKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (a *app) importIMAP(ctx context.Context, opt imapImportOptions, out io.Writer) error {
	if _, err := a.baseAccountHash(ctx, a.db, opt.owner); err != nil {
		return errors.New("destination must be an active primary mailbox")
	}
	f := os.Stdin
	if opt.passwordFile != "-" {
		var err error
		f, err = os.Open(opt.passwordFile)
		if err != nil {
			return errors.New("could not open source password file")
		}
		defer f.Close()
	}
	credential, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(credential) == 0 || len(credential) > 4096 {
		return errors.New("could not read source password")
	}
	password := strings.TrimSuffix(strings.TrimSuffix(string(credential), "\n"), "\r")
	if password == "" || strings.ContainsAny(password, "\r\n\x00") {
		return errors.New("source password must be one non-empty line")
	}
	host, _, _ := net.SplitHostPort(opt.host)
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if opt.caFile != "" {
		pem, err := os.ReadFile(opt.caFile)
		if err != nil {
			return errors.New("cannot read source PEM CA")
		}
		config.RootCAs, err = x509.SystemCertPool()
		if err != nil || config.RootCAs == nil {
			config.RootCAs = x509.NewCertPool()
		}
		if !config.RootCAs.AppendCertsFromPEM(pem) {
			return errors.New("source PEM CA does not contain a certificate")
		}
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 15 * time.Second}, Config: config}
	conn, err := dialer.DialContext(ctx, "tcp", opt.host)
	if err != nil {
		return fmt.Errorf("connect to source IMAPS: %w", err)
	}
	defer conn.Close()
	cancelRead := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer cancelRead()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	source, err := client.New(conn)
	if err != nil {
		return errors.New("source IMAP greeting failed")
	}
	source.Timeout = 90 * time.Second
	if err := source.Login(opt.user, password); err != nil {
		return errors.New("source IMAP authentication failed")
	}
	if err := a.copyIMAPMailboxes(ctx, source, opt, out); err != nil {
		return err
	}
	source.Timeout = 3 * time.Second
	_ = source.Logout()
	return nil
}

type imapImportFolder struct {
	name, label, key string
}

func (a *app) copyIMAPMailboxes(ctx context.Context, source *client.Client, opt imapImportOptions, out io.Writer) error {
	folders := make(chan *imap.MailboxInfo)
	done := make(chan error, 1)
	go func() { done <- source.List("", "*", folders) }()
	var selected []imapImportFolder
	paths := make(map[string]string)
	var collision bool
	for folder := range folders {
		if slices.Contains(folder.Attributes, imap.NoSelectAttr) || slices.Contains(folder.Attributes, "\\NonExistent") {
			continue
		}
		key := imapImportKey(opt.host, opt.user, opt.owner, opt.root, folder.Name)
		label := imapImportLabel(opt.root, folder.Name, folder.Delimiter)
		lookup := strings.ToLower(label)
		if previous, ok := paths[lookup]; ok && previous != folder.Name {
			collision = true
		}
		paths[lookup] = folder.Name
		selected = append(selected, imapImportFolder{folder.Name, label, key})
	}
	if err := <-done; err != nil {
		return errors.New("could not list source IMAP folders")
	}
	if collision {
		return errors.New("source folders map to the same Harbor folder")
	}
	slices.SortFunc(selected, func(x, y imapImportFolder) int { return strings.Compare(x.label, y.label) })
	for _, folder := range selected {
		count, err := a.copyIMAPFolder(ctx, source, opt.owner, folder)
		fmt.Fprintf(out, "%q → %q: %d committed\n", folder.name, folder.label, count)
		if err != nil {
			return fmt.Errorf("folder %q: %w", folder.name, err)
		}
	}
	return nil
}

func imapImportLabel(root, name, delimiter string) string {
	parts := []string{name}
	if delimiter != "" {
		parts = strings.Split(name, delimiter)
	}
	for i, part := range parts {
		if strings.TrimSpace(part) == "" || strings.TrimSpace(part) != part ||
			strings.ContainsAny(part, "/\\*%") || strings.ContainsFunc(part, unicode.IsControl) {
			parts[i] = "目录-" + imapImportKey(part)[:12]
		}
	}
	label, err := folderName(root + "/" + strings.Join(parts, "/"))
	if err != nil {
		return root + "/目录-" + imapImportKey(name)[:12]
	}
	return label
}

func (a *app) copyIMAPFolder(ctx context.Context, source *client.Client, owner string, folder imapImportFolder) (int, error) {
	status, err := source.Select(folder.name, true)
	if err != nil || status.UidValidity == 0 || status.UidNext == 0 {
		return 0, errors.New("source did not provide valid UIDVALIDITY and UIDNEXT")
	}
	last, err := a.startIMAPImport(ctx, owner, folder, status.UidValidity)
	if err != nil || last >= status.UidNext-1 {
		return 0, err
	}
	criteria := imap.NewSearchCriteria()
	criteria.Uid = new(imap.SeqSet)
	criteria.Uid.AddRange(last+1, status.UidNext-1)
	uids, err := source.UidSearch(criteria)
	if err != nil {
		return 0, errors.New("could not search source IMAP UIDs")
	}
	slices.Sort(uids)
	count := 0
	for _, uid := range uids {
		if uid <= last || uid >= status.UidNext {
			continue
		}
		raw, m, err := fetchIMAPImport(source, uid, a.messageLimit())
		if err != nil {
			return count, fmt.Errorf("UID %d: %w", uid, err)
		}
		m.To = owner
		added, err := a.saveIMAPImport(ctx, owner, folder.key, status.UidValidity, uid, m, raw)
		if err != nil {
			return count, fmt.Errorf("UID %d: %w", uid, err)
		}
		if added {
			count++
		}
		last = uid
	}
	return count, nil
}

func fetchIMAPImport(source *client.Client, uid uint32, limit int) ([]byte, letter, error) {
	set := new(imap.SeqSet)
	set.AddNum(uid)
	section := &imap.BodySectionName{Peek: true, Partial: []int{0, limit + 1}}
	messages := make(chan *imap.Message)
	done := make(chan error, 1)
	go func() {
		done <- source.UidFetch(set, []imap.FetchItem{imap.FetchUid, imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size, section.FetchItem()}, messages)
	}()
	var raw []byte
	var msg *imap.Message
	var readErr error
	for result := range messages {
		if result.Uid != uid {
			continue
		}
		body := result.GetBody(section)
		if body == nil || result.Size == 0 || result.Size > uint32(limit) {
			continue
		}
		raw, readErr = io.ReadAll(io.LimitReader(body, int64(limit)+1))
		msg = result
	}
	if err := <-done; err != nil {
		return nil, letter{}, errors.New("source IMAP FETCH failed")
	}
	if readErr != nil || msg == nil || len(raw) > limit || len(raw) != int(msg.Size) {
		return nil, letter{}, errors.New("source message is unavailable, truncated, or exceeds the configured size limit")
	}
	m := interpret(raw, "", "")
	if !msg.InternalDate.IsZero() {
		m.Date = msg.InternalDate.Unix()
	}
	for _, original := range msg.Flags {
		switch imap.CanonicalFlag(original) {
		case imap.SeenFlag:
			m.Read = true
		case imap.FlaggedFlag:
			m.Star = true
		}
	}
	return raw, m, nil
}

func (a *app) startIMAPImport(ctx context.Context, owner string, folder imapImportFolder, validity uint32) (uint32, error) {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return 0, err
	}
	if _, err := a.baseAccountHash(ctx, tx, owner); err != nil {
		return 0, err
	}
	if err := a.requireActiveAccount(ctx, tx, owner); err != nil {
		return 0, err
	}
	var savedValidity, last uint32
	var id string
	err = tx.QueryRowContext(ctx, "SELECT folder,uid_validity,last_uid FROM imap_import_checkpoints WHERE source_key=? AND owner=?",
		folder.key, owner).Scan(&id, &savedValidity, &last)
	if err == nil {
		if savedValidity != validity {
			return 0, errors.New("source UIDVALIDITY changed; restart under a new destination folder after reconciling the previous copy")
		}
		return last, verifyFolder(ctx, tx, owner, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	folders, err := listCustomFolders(ctx, tx, owner)
	if err != nil {
		return 0, err
	}
	byPath := make(map[string]customFolder, len(folders))
	for _, current := range folders {
		byPath[strings.ToLower(current.Label)] = current
	}
	label, err := ensureFolderParents(ctx, tx, owner, folder.label, byPath)
	if err != nil {
		return 0, err
	}
	id = byPath[strings.ToLower(label)].ID
	if id == "" {
		id = "u" + token()[:15]
		if _, err := tx.ExecContext(ctx, "INSERT INTO custom_folders(folder,owner,label,lookup) VALUES(?,?,?,?)",
			id, owner, label, strings.ToLower(label)); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO imap_import_checkpoints(source_key,owner,folder,uid_validity,last_uid) VALUES(?,?,?,?,0)",
		folder.key, owner, id, validity); err != nil {
		return 0, err
	}
	return 0, a.commitMailbox(tx, owner)
}

func (a *app) saveIMAPImport(ctx context.Context, owner, key string, validity, uid uint32, m letter, raw []byte) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := lockIMAPUIDs(ctx, tx); err != nil {
		return false, err
	}
	if _, err := a.baseAccountHash(ctx, tx, owner); err != nil {
		return false, err
	}
	if err := a.requireActiveAccount(ctx, tx, owner); err != nil {
		return false, err
	}
	var id string
	var last uint32
	if err := tx.QueryRowContext(ctx, "SELECT folder,last_uid FROM imap_import_checkpoints WHERE source_key=? AND owner=? AND uid_validity=?",
		key, owner, validity).Scan(&id, &last); err != nil {
		return false, err
	}
	if last >= uid {
		return false, nil
	}
	if err := verifyFolder(ctx, tx, owner, id); err != nil {
		return false, err
	}
	if _, err := insertLetter(ctx, tx, owner, m, id, "inbox", raw); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE imap_import_checkpoints SET last_uid=? WHERE source_key=?", uid, key); err != nil {
		return false, err
	}
	if err := a.commitMailbox(tx, owner); err != nil {
		return false, err
	}
	return true, nil
}
