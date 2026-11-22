package server

import (
	"database/sql"
	"html/template"
	"net/http"
	"strings"
	"time"
)

var letterPrintPage = template.Must(template.New("letter-print").Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Subject}} · Harbor Mail</title><link rel="stylesheet" href="/letter-print.css">
<script src="/letter-print.js" defer></script></head>
<body data-auto-print="{{.AutoPrint}}">
<main><button id="print" type="button">打印邮件</button>
<header><h1>{{.Subject}}</h1>
<dl><dt>发件人</dt><dd>{{.From}}</dd>
<dt>收件人</dt><dd>{{.To}}</dd>
{{if .Cc}}<dt>抄送</dt><dd>{{.Cc}}</dd>{{end}}
{{if .Bcc}}<dt>密送</dt><dd>{{.Bcc}}</dd>{{end}}
<dt>日期</dt><dd><time datetime="{{.Timestamp}}">{{.Date}}</time></dd></dl></header>
{{if .HTML}}<div class="rich">{{.HTML}}</div>{{else}}<div class="plain">{{.Body}}</div>{{end}}
</main></body></html>`))

func (a *app) printLetter(w http.ResponseWriter, r *http.Request, owner string) {
	var m letter
	var to sql.NullString
	var raw []byte
	err := a.db.QueryRowContext(r.Context(), `SELECT l.sender,l.recipient,l.subject,l.body,l.stamp,l.raw,e.to_list,
		coalesce(e.cc_list,''),coalesce(e.bcc_list,'') FROM letters l
		LEFT JOIN web_letter_recipients e ON e.letter_id=l.id WHERE l.owner=? AND l.id=?`,
		owner, letterID(r)).Scan(&m.From, &m.To, &m.Subject, &m.Body, &m.Date, &raw, &to, &m.Cc, &m.Bcc)
	if letterError(w, err) {
		return
	}
	if to.Valid {
		m.To = to.String
	} else {
		m.Cc = visibleRecipients(raw).Cc
	}
	if strings.TrimSpace(m.Subject) == "" {
		m.Subject = "（无主题）"
	}
	if m.From == "" {
		m.From = "退信"
	}
	page := struct {
		Subject, From, To, Cc, Bcc, Body, Date, Timestamp string
		HTML                                              template.HTML
		AutoPrint                                         bool
	}{
		Subject: m.Subject, From: displaySender(m.From), To: m.To, Cc: m.Cc, Bcc: m.Bcc, Body: m.Body,
		AutoPrint: r.URL.Query().Get("print") == "1",
	}
	date := time.Unix(m.Date, 0).UTC()
	page.Timestamp, page.Date = date.Format(time.RFC3339), date.Format("2006-01-02 15:04 UTC")
	if mode := r.URL.Query().Get("format"); mode == "html" || (mode != "text" && strings.TrimSpace(m.Body) == "") {
		view := inspectHTMLMail(raw, true)
		if view.hasHTML {
			fragment, err := view.renderFragment()
			if letterError(w, err) {
				return
			}
			// Only the shared sanitized renderer can supply markup to the print page.
			page.HTML = template.HTML(fragment)
			if view.tooLong && strings.TrimSpace(m.Body) != "" {
				page.HTML = ""
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
		"img-src data:; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	_ = letterPrintPage.Execute(w, page)
}
