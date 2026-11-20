package server

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	message "github.com/emersion/go-message/mail"
)

var composeInline = regexp.MustCompile(`\*\*([^*\r\n]+)\*\*|\[([^\]\r\n]+)\]\(([^\s)]+)\)`)

func renderRichCompose(source string) (string, string) {
	var plain, rich strings.Builder
	open := ""
	for i, line := range strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n") {
		if i > 0 {
			plain.WriteByte('\n')
		}
		block, body := "", line
		switch {
		case strings.HasPrefix(line, "- "):
			block, body = "ul", line[2:]
			plain.WriteString("- ")
		case strings.HasPrefix(line, "> "):
			block, body = "blockquote", line[2:]
			plain.WriteString("> ")
		}
		if open != block {
			if open != "" {
				rich.WriteString("</" + open + ">")
			}
			if block != "" {
				rich.WriteString("<" + block + ">")
			}
			open = block
		}
		tag := "p"
		if block == "ul" {
			tag = "li"
		}
		if body == "" && block == "" {
			rich.WriteString("<br>")
			continue
		}
		rich.WriteString("<" + tag + ">")
		renderComposeInline(&plain, &rich, body)
		rich.WriteString("</" + tag + ">")
	}
	if open != "" {
		rich.WriteString("</" + open + ">")
	}
	return plain.String(), rich.String()
}

func renderComposeInline(plain, rich *strings.Builder, body string) {
	write := func(text string) {
		plain.WriteString(text)
		rich.WriteString(html.EscapeString(text))
	}
	last := 0
	for _, index := range composeInline.FindAllStringSubmatchIndex(body, -1) {
		write(body[last:index[0]])
		if index[2] >= 0 {
			rich.WriteString("<strong>")
			write(body[index[2]:index[3]])
			rich.WriteString("</strong>")
		} else {
			label, target := body[index[4]:index[5]], body[index[6]:index[7]]
			link, err := url.Parse(target)
			if err == nil && ((link.Scheme == "https" || link.Scheme == "http") && link.Host != "" ||
				link.Scheme == "mailto" && link.Opaque != "") {
				rich.WriteString(`<a href="` + html.EscapeString(target) + `">`)
				write(label)
				rich.WriteString("</a>")
				plain.WriteString(" (" + target + ")")
			} else {
				write(body[index[0]:index[1]])
			}
		}
		last = index[1]
	}
	write(body[last:])
}

func writeRichCompose(writer *message.InlineWriter, source string) error {
	plain, rich := renderRichCompose(source)
	for _, part := range []struct{ kind, body string }{{"text/plain", plain}, {"text/html", rich}} {
		var header message.InlineHeader
		header.Set("Content-Type", part.kind+"; charset=UTF-8")
		target, err := writer.CreatePart(header)
		if err != nil {
			return err
		}
		if err := writeMIMEPart(target, []byte(part.body)); err != nil {
			return err
		}
	}
	return writer.Close()
}

func (a *app) previewCompose(w http.ResponseWriter, r *http.Request, _ string) {
	var input struct{ Body string }
	if !decode(w, r, &input) {
		return
	}
	if len(input.Body) > 256<<10 {
		fail(w, http.StatusBadRequest, "正文超过 256 KiB")
		return
	}
	_, markup := renderRichCompose(input.Body)
	w.Header().Set("Cache-Control", "no-store")
	reply(w, http.StatusOK, map[string]string{"html": markup})
}
