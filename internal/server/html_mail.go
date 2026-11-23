package server

import (
	"io"
	"net/http"

	"harbormail/internal/mailmime"
)

const emailFramePolicy = "default-src 'none'; img-src data:; style-src 'unsafe-inline'; " +
	"frame-ancestors 'self'; base-uri 'none'; form-action 'none'; sandbox allow-popups allow-popups-to-escape-sandbox"

func (a *app) htmlLetter(w http.ResponseWriter, r *http.Request, owner string) {
	raw, err := a.rawLetter(owner, letterID(r))
	if letterError(w, err) {
		return
	}
	view := mailmime.InspectHTML(raw, true)
	if !view.HasHTML {
		fail(w, 404, "此邮件没有 HTML 正文")
		return
	}
	fragment, err := view.RenderFragment()
	if letterError(w, err) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", emailFramePolicy)
	w.Header().Set("Referrer-Policy", "no-referrer")
	// The fragment was sanitized and all embedded image URLs were generated above.
	_, _ = io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8">
		<meta name="viewport" content="width=device-width, initial-scale=1">
		<title>HTML 邮件正文</title>
		<style>body{font:15px/1.65 system-ui,sans-serif;color:#182640;padding:16px;margin:0;overflow-wrap:anywhere}
		img{max-width:100%;height:auto}pre{white-space:pre-wrap}a{color:#234b9a}</style></head><body>`)
	_, _ = w.Write(fragment)
	_, _ = io.WriteString(w, "</body></html>")
}
