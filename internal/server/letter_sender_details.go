package server

import (
	"net/http"
	"net/url"
	"strings"
)

type letterSenderDetails struct {
	ReturnPath    string `json:"returnPath,omitempty"`
	SigningDomain string `json:"signingDomain,omitempty"`
	MessageID     string `json:"messageId,omitempty"`
	ListID        string `json:"listId,omitempty"`
	Unsubscribe   string `json:"unsubscribe,omitempty"`
}

func senderDetailsFromHeader(raw []byte) letterSenderDetails {
	header := letterHeader(raw)
	details := letterSenderDetails{}
	path := strings.TrimSpace(header.Get("Return-Path"))
	if path == "<>" {
		details.ReturnPath = path
	} else if addr, err := address(strings.Trim(path, "<>")); err == nil && len(addr) <= 254 {
		details.ReturnPath = addr
	}
	for _, field := range strings.Split(header.Get("DKIM-Signature"), ";") {
		tag, value, ok := strings.Cut(field, "=")
		if ok && strings.TrimSpace(tag) == "d" {
			domain := strings.ToLower(strings.TrimSpace(value))
			if strings.Contains(domain, ".") && validDNSName(domain) {
				details.SigningDomain = domain
			}
			break
		}
	}
	if id, err := header.MessageID(); err == nil && usableMessageID(id) {
		details.MessageID = "<" + id + ">"
	}
	_, listID, _ := strings.Cut(header.Get("List-Id"), "<")
	if id, _, ok := strings.Cut(listID, ">"); ok && len(id) <= 254 && usableMessageID(id) {
		details.ListID = id
	}
	details.Unsubscribe = unsubscribeLink(header.Get("List-Unsubscribe"))
	return details
}

func unsubscribeLink(value string) string {
	for {
		_, rest, ok := strings.Cut(value, "<")
		if !ok {
			return ""
		}
		var candidate string
		candidate, value, ok = strings.Cut(rest, ">")
		if !ok {
			return ""
		}
		link, err := url.Parse(strings.TrimSpace(candidate))
		if err == nil && len(candidate) <= 2048 && link.Scheme == "https" && link.Hostname() != "" && link.User == nil {
			return link.String()
		}
	}
}

func (a *app) readLetterSenderDetails(w http.ResponseWriter, r *http.Request, owner string) {
	var raw []byte
	err := a.db.QueryRowContext(r.Context(), "SELECT SUBSTR(raw,1,?) FROM letters WHERE owner=? AND id=?",
		threadHeaderLimit, owner, letterID(r)).Scan(&raw)
	if letterError(w, err) {
		return
	}
	reply(w, http.StatusOK, senderDetailsFromHeader(raw))
}
