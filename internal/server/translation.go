package server

import (
	"bytes"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	message "github.com/emersion/go-message/mail"
	"golang.org/x/net/html"

	"harbormail/internal/mailmime"
)

const translationCharLimit = 20000

func emailTranslationBody(raw []byte) (string, error) {
	reader, _ := message.CreateReader(bytes.NewReader(raw))
	if reader == nil {
		return "", nil
	}
	defer reader.Close()
	var rich []byte
	for {
		part, _ := reader.NextPart()
		if part == nil {
			break
		}
		header, ok := part.Header.(*message.InlineHeader)
		if !ok {
			continue
		}
		media, _, _ := header.ContentType()
		attachment := message.AttachmentHeader{Header: header.Header}
		filename, _ := attachment.Filename()
		if filename != "" || (media != "text/plain" && (media != "text/html" || rich != nil)) {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(part.Body, mailmime.HTMLBodyLimit+1))
		if err != nil {
			return "", err
		}
		if media == "text/plain" {
			if len(body) > mailmime.HTMLBodyLimit {
				return "", errTranslationTooLong
			}
			if plain := strings.TrimSpace(strings.ToValidUTF8(string(body), "\uFFFD")); plain != "" {
				return plain, nil
			}
		} else {
			rich = body
		}
	}
	if len(rich) > mailmime.HTMLBodyLimit {
		return "", errTranslationTooLong
	}
	var plain strings.Builder
	tokens := html.NewTokenizer(bytes.NewReader(mailmime.SanitizeHTML(rich)))
	for {
		switch tokens.Next() {
		case html.ErrorToken:
			return strings.TrimSpace(plain.String()), nil
		case html.TextToken:
			plain.Write(tokens.Text())
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			name, _ := tokens.TagName()
			switch string(name) {
			case "br", "p", "div", "li", "tr", "blockquote", "h1", "h2", "h3", "h4", "h5", "h6":
				if plain.Len() > 0 && plain.String()[plain.Len()-1] != '\n' {
					plain.WriteByte('\n')
				}
			case "td", "th":
				plain.WriteByte(' ')
			}
		}
	}
}

var errTranslationTooLong = errors.New("email is too long to translate")

type translatedEmail struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func validTranslationLanguage(language string) bool {
	switch language {
	case "zh-Hans", "zh-Hant", "en", "ja", "ko", "fr", "de", "es":
		return true
	default:
		return false
	}
}

func translationSource(w http.ResponseWriter, subject string, raw []byte) (translatedEmail, bool) {
	body, err := emailTranslationBody(raw)
	original := translatedEmail{Subject: strings.TrimSpace(subject), Body: body}
	if errors.Is(err, errTranslationTooLong) || utf8.RuneCountInString(original.Subject)+utf8.RuneCountInString(original.Body) > translationCharLimit {
		fail(w, http.StatusRequestEntityTooLarge, "邮件主题和正文合计超过 20,000 字符，无法翻译。")
		return translatedEmail{}, false
	}
	if err != nil || (original.Subject == "" && original.Body == "") {
		fail(w, http.StatusUnprocessableEntity, "这封邮件没有可翻译的文本。")
		return translatedEmail{}, false
	}
	return original, true
}

func (a *app) translateLetter(w http.ResponseWriter, r *http.Request, owner string) {
	if a.translator == nil {
		fail(w, http.StatusServiceUnavailable, "此服务器尚未配置邮件翻译。")
		return
	}
	var input struct {
		Target   string `json:"target"`
		Provider string `json:"provider"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !validTranslationLanguage(input.Target) {
		fail(w, http.StatusBadRequest, "请选择支持的目标语言。")
		return
	}

	if input.Provider == "" {
		if ids := a.translator.providerIDs(); len(ids) > 0 {
			input.Provider = ids[0]
		}
	}
	translate := a.translator.providers[input.Provider]
	if translate == nil {
		fail(w, http.StatusBadRequest, "请选择已启用的翻译服务。")
		return
	}

	select {
	case a.translator.slots <- struct{}{}:
		defer func() { <-a.translator.slots }()
	default:
		fail(w, http.StatusTooManyRequests, "翻译服务繁忙，请稍后再试。")
		return
	}
	var sourceSubject string
	var raw []byte
	err := a.db.QueryRowContext(r.Context(), `SELECT subject,raw FROM letters WHERE owner=? AND id=? AND folder<>'drafts'`, owner, letterID(r)).Scan(&sourceSubject, &raw)
	if letterError(w, err) {
		return
	}
	key := mailTranslationKey(owner, letterID(r), input.Provider, input.Target, sourceSubject, raw)
	if result, ok := a.cachedTranslation(r.Context(), key); ok {
		reply(w, http.StatusOK, result)
		return
	}
	original, ok := translationSource(w, sourceSubject, raw)
	if !ok {
		return
	}
	var texts []string
	for _, text := range []string{original.Subject, original.Body} {
		if text != "" {
			texts = append(texts, text)
		}
	}
	result, status := translate(r.Context(), input.Target, texts)
	if status == http.StatusTooManyRequests {
		fail(w, status, "所选翻译服务达到额度或频率限制，请稍后再试。")
		return
	}
	if status != http.StatusOK || len(result) != len(texts) {
		fail(w, http.StatusBadGateway, "所选翻译服务无法完成请求，请稍后重试。")
		return
	}
	for _, text := range result {
		if strings.TrimSpace(text) == "" {
			fail(w, http.StatusBadGateway, "所选翻译服务返回空译文。")
			return
		}
	}
	translated := translatedEmail{}
	if original.Subject != "" {
		translated.Subject = result[0]
	}
	if original.Body != "" {
		translated.Body = result[len(result)-1]
	}
	if err := a.storeTranslation(r.Context(), key, sourceSubject, raw, translated); errors.Is(err, sql.ErrNoRows) {
		letterError(w, err)
		return
	}
	reply(w, http.StatusOK, translated)
}
