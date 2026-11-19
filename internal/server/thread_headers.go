package server

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"io"
	"slices"
	"strings"

	"github.com/emersion/go-message"
	mail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

type replyHeaders struct {
	parents    []string
	references []string
}

type messageIdentity struct {
	id string
	replyHeaders
}

const threadHeaderLimit = 256 << 10

func letterHeader(raw []byte) mail.Header {
	header, err := textproto.ReadHeader(bufio.NewReader(io.LimitReader(bytes.NewReader(raw), threadHeaderLimit)))
	if err != nil {
		return mail.Header{}
	}
	return mail.Header{Header: message.Header{Header: header}}
}

func usableMessageID(id string) bool {
	return id != "" && len(id) <= 512 && !strings.ContainsFunc(id, func(r rune) bool {
		return r <= ' ' || r >= 127 || r == '<' || r == '>'
	})
}

func boundedMessageIDs(values []string) []string {
	const maxIDs = 32
	seen := make(map[string]bool)
	var result []string
	for _, id := range values {
		if usableMessageID(id) && !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	if len(result) > maxIDs {
		result = append(result[:1], result[len(result)-maxIDs+1:]...)
	}
	return result
}

func identifyMessage(raw []byte) messageIdentity {
	header := letterHeader(raw)
	id, _ := header.MessageID()
	if !usableMessageID(id) {
		id = ""
	}
	parents, _ := header.MsgIDList("In-Reply-To")
	references, _ := header.MsgIDList("References")
	return messageIdentity{id: id, replyHeaders: replyHeaders{
		parents: boundedMessageIDs(parents), references: boundedMessageIDs(references),
	}}
}

func prepareReply(ctx context.Context, q folderQueryer, owner string, id int64) (replyHeaders, error) {
	if id == 0 {
		return replyHeaders{}, nil
	}
	if id < 0 {
		return replyHeaders{}, sql.ErrNoRows
	}
	var raw []byte
	if err := q.QueryRowContext(ctx, `SELECT substr(raw,1,?) FROM letters WHERE owner=? AND id=?`,
		threadHeaderLimit, owner, id).Scan(&raw); err != nil {
		return replyHeaders{}, err
	}
	parent := identifyMessage(raw)
	references := parent.references
	if len(references) == 0 && len(parent.parents) == 1 {
		references = slices.Clone(parent.parents)
	}
	var parents []string
	if parent.id != "" {
		parents = []string{parent.id}
		// Put the immediate parent last even if the upstream chain repeated it.
		references = slices.DeleteFunc(references, func(id string) bool { return id == parent.id })
		references = append(references, parent.id)
	}
	return replyHeaders{parents: parents, references: boundedMessageIDs(references)}, nil
}

func replyAddress(raw []byte, fallback string) string {
	header := letterHeader(raw)
	recipients, _ := header.AddressList("Reply-To")
	for _, recipient := range recipients {
		if reply, err := address(recipient.Address); err == nil {
			return reply
		}
	}
	resolved, _ := address(fallback)
	return resolved
}
