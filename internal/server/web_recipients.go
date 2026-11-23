package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/mail"
	"strings"

	message "github.com/emersion/go-message/mail"
)

const maxWebRecipients = 20
const maxRecipientField = maxWebRecipients * 256

type webRecipients struct {
	To, Cc, Bcc string
}

type webTarget struct {
	Address string
	Owner   string
}

type webEnvelope struct {
	Recipients webRecipients
	Targets    []webTarget
	Author     string `json:",omitempty"`
	Formatted  bool   `json:",omitempty"`
}

var errInvalidWebRecipients = errors.New("invalid web recipients")

func initWebRecipientSchema(ctx context.Context, db *sql.DB, driver string) error {
	if driver == "mysql" {
		for _, statement := range []string{
			`CREATE TABLE IF NOT EXISTS web_letter_recipients (
				letter_id BIGINT PRIMARY KEY, to_list TEXT NOT NULL, cc_list TEXT NOT NULL, bcc_list TEXT NOT NULL
			) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
			`CREATE TABLE IF NOT EXISTS web_outbox_envelopes (
				outbox_id BIGINT PRIMARY KEY, envelope TEXT NOT NULL,
				has_external BOOLEAN NOT NULL, local_done BOOLEAN NOT NULL DEFAULT FALSE
			) ENGINE=InnoDB DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`,
		} {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		return nil
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS web_letter_recipients (
			letter_id INTEGER PRIMARY KEY, to_list TEXT NOT NULL, cc_list TEXT NOT NULL, bcc_list TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS web_outbox_envelopes (
			outbox_id INTEGER PRIMARY KEY, envelope TEXT NOT NULL,
			has_external BOOLEAN NOT NULL, local_done BOOLEAN NOT NULL DEFAULT FALSE
		)`,
		`CREATE TRIGGER IF NOT EXISTS delete_web_letter_recipients AFTER DELETE ON letters
			BEGIN DELETE FROM web_letter_recipients WHERE letter_id=OLD.id; END`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (input outgoingLetter) recipientFields() webRecipients {
	return webRecipients{strings.TrimSpace(input.To), strings.TrimSpace(input.Cc), strings.TrimSpace(input.Bcc)}
}

func (r webRecipients) validFields() bool {
	for _, field := range []string{r.To, r.Cc, r.Bcc} {
		if len(field) > maxRecipientField || strings.ContainsAny(field, "\r\n\x00") {
			return false
		}
	}
	return true
}

func recipientTokens(field string) []string {
	return strings.FieldsFunc(field, func(r rune) bool { return r == ',' || r == ';' || r == '，' || r == '；' })
}

func (a *app) resolveWebEnvelope(ctx context.Context, db folderQueryer, fields webRecipients) (webEnvelope, error) {
	var envelope webEnvelope
	if !fields.validFields() {
		return envelope, errInvalidWebRecipients
	}
	seen := make(map[string]bool)
	inputs := []string{fields.To, fields.Cc, fields.Bcc}
	outputs := []*string{&envelope.Recipients.To, &envelope.Recipients.Cc, &envelope.Recipients.Bcc}
	for i, field := range inputs {
		var list []string
		for _, token := range recipientTokens(field) {
			if strings.TrimSpace(token) == "" {
				continue
			}
			to, err := address(token)
			if err != nil {
				return webEnvelope{}, errInvalidWebRecipients
			}
			target, err := a.destination(ctx, db, to)
			if err != nil {
				return webEnvelope{}, err
			}
			if seen[target.address] {
				continue
			}
			seen[target.address] = true
			list = append(list, target.address)
			envelope.Targets = append(envelope.Targets, webTarget{Address: target.address, Owner: target.owner})
			if len(envelope.Targets) > maxWebRecipients {
				return webEnvelope{}, errInvalidWebRecipients
			}
		}
		*outputs[i] = strings.Join(list, ", ")
	}
	return envelope, nil
}

func (r webRecipients) indexedAddress() string {
	for _, field := range []string{r.To, r.Cc} {
		for _, token := range recipientTokens(field) {
			if to, err := address(token); err == nil {
				return to
			}
		}
	}
	return ""
}

func setVisibleRecipients(header *message.Header, fields webRecipients) {
	for _, field := range []struct{ key, value string }{{"To", fields.To}, {"Cc", fields.Cc}} {
		var list []*mail.Address
		for _, token := range recipientTokens(field.value) {
			if email, err := address(token); err == nil {
				list = append(list, &mail.Address{Address: email})
			}
		}
		if len(list) != 0 {
			header.SetAddressList(field.key, list)
		}
	}
}

func (e webEnvelope) externalAddresses() []string {
	var addresses []string
	for _, target := range e.Targets {
		if target.Owner == "" {
			addresses = append(addresses, target.Address)
		}
	}
	return addresses
}

func (m *letter) showWebRecipients(fields webRecipients) {
	m.To, m.Cc, m.Bcc = fields.To, fields.Cc, fields.Bcc
}

func saveWebRecipients(ctx context.Context, tx *sql.Tx, id int64, fields webRecipients) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO web_letter_recipients(letter_id,to_list,cc_list,bcc_list) VALUES(?,?,?,?)`,
		id, fields.To, fields.Cc, fields.Bcc)
	return err
}

func parseWebEnvelope(data string) (*webEnvelope, error) {
	if data == "" {
		return nil, nil
	}
	var envelope webEnvelope
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		return nil, err
	}
	if len(envelope.Targets) == 0 {
		return nil, errInvalidWebRecipients
	}
	return &envelope, nil
}

func saveWebEnvelope(ctx context.Context, tx *sql.Tx, id int64, envelope webEnvelope) error {
	data, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO web_outbox_envelopes(outbox_id,envelope,has_external) VALUES(?,?,?)`,
		id, string(data), len(envelope.externalAddresses()) != 0)
	return err
}

func deleteWebEnvelope(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM web_outbox_envelopes WHERE outbox_id=?`, id)
	return err
}
