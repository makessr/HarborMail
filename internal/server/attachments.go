package server

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/http"
	"path"
	"strconv"
	"strings"
	"unicode"

	"github.com/emersion/go-message"
	mail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

const (
	attachmentLimit = 6 << 20
	attachmentCount = 10
)

type attachmentInfo struct {
	Part int    `json:"part"`
	Name string `json:"name"`
}

type uploadedAttachment struct {
	name string
	data []byte
}

type outgoingLetter struct {
	To, Cc, Bcc   string
	Subject, Body string
	ReplyToID     int64 `json:"replyToId"`
	files         []uploadedAttachment
}

func attachmentName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name))
	for i := range name {
		if i > 150 {
			name = name[:i]
			break
		}
	}
	if name == "" || name == "." || name == ".." || name == "/" {
		return "attachment"
	}
	return name
}

func decodeOutgoing(w http.ResponseWriter, r *http.Request, input *outgoingLetter) bool {
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if media != "multipart/form-data" {
		return decode(w, r, input)
	}
	r.Body = http.MaxBytesReader(w, r.Body, attachmentLimit+(512<<10))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, 413, "附件总大小上限为 6 MiB")
		} else {
			fail(w, 400, "无法读取上传内容")
		}
		return false
	}
	defer r.MultipartForm.RemoveAll()
	input.To = r.PostForm.Get("to")
	input.Cc = r.PostForm.Get("cc")
	input.Bcc = r.PostForm.Get("bcc")
	input.Subject = r.PostForm.Get("subject")
	input.Body = r.PostForm.Get("body")
	if value := r.PostForm.Get("replyToId"); value != "" {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id < 1 {
			fail(w, 400, "无法识别要回复的邮件")
			return false
		}
		input.ReplyToID = id
	}
	files := r.MultipartForm.File["attachments"]
	if len(files) > attachmentCount {
		fail(w, 413, "每封邮件最多可添加 10 个附件")
		return false
	}
	remaining := int64(attachmentLimit)
	for _, file := range files {
		if file.Size > remaining {
			fail(w, 413, "附件总大小上限为 6 MiB")
			return false
		}
		part, err := file.Open()
		if err != nil {
			fail(w, 400, "无法读取附件")
			return false
		}
		data, err := io.ReadAll(io.LimitReader(part, remaining+1))
		_ = part.Close()
		if err != nil {
			fail(w, 400, "无法读取附件")
			return false
		}
		remaining -= int64(len(data))
		if remaining < 0 {
			fail(w, 413, "附件总大小上限为 6 MiB")
			return false
		}
		input.files = append(input.files, uploadedAttachment{attachmentName(file.Filename), data})
	}
	return true
}

// Use the immutable MIME message as the source of attachment identifiers.
func visitAttachments(raw []byte, visit func(attachmentInfo, io.Reader) bool) {
	body := bufio.NewReader(bytes.NewReader(raw))
	header, err := textproto.ReadHeader(body)
	if err != nil {
		return
	}
	index := 0
	walkAttachments(header, body, 0, func(name string, data io.Reader) bool {
		index++
		return visit(attachmentInfo{Part: index, Name: attachmentName(name)}, data)
	})
}

func walkAttachments(rawHeader textproto.Header, body io.Reader, depth int, visit func(string, io.Reader) bool) bool {
	header := mail.AttachmentHeader{Header: message.Header{Header: rawHeader}}
	kind, params, _ := header.ContentType()
	disposition, _, _ := header.ContentDisposition()
	name, _ := header.Filename()
	encoding := strings.ToLower(strings.TrimSpace(header.Get("Content-Transfer-Encoding")))
	switch encoding {
	case "", "7bit", "8bit", "binary":
	case "base64":
		body = base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		body = quotedprintable.NewReader(body)
	default:
		body = nil
	}
	if disposition == "attachment" || name != "" {
		return visit(name, body)
	}
	if strings.HasPrefix(kind, "multipart/") {
		boundary := params["boundary"]
		if depth >= 32 || boundary == "" || body == nil {
			return true
		}
		parts := textproto.NewMultipartReader(body, boundary)
		for {
			part, err := parts.NextPart()
			if err != nil {
				return true
			}
			if !walkAttachments(part.Header, part, depth+1, visit) {
				return false
			}
		}
	}
	if disposition != "inline" && !strings.HasPrefix(kind, "text/") {
		return visit("", body)
	}
	return true
}

func (a *app) downloadAttachment(w http.ResponseWriter, r *http.Request, owner string) {
	index, err := strconv.Atoi(r.PathValue("part"))
	if err != nil || index < 1 {
		fail(w, 404, "找不到此附件")
		return
	}
	raw, err := a.rawLetter(owner, letterID(r))
	if letterError(w, err) {
		return
	}
	found := false
	visitAttachments(raw, func(info attachmentInfo, body io.Reader) bool {
		if info.Part != index {
			return true
		}
		found = true
		if body == nil {
			fail(w, 422, "无法解码此附件。可下载原始邮件。")
			return false
		}
		data, err := io.ReadAll(io.LimitReader(body, mailLimit+1))
		if err != nil || len(data) > mailLimit {
			fail(w, 422, "无法解码此附件。可下载原始邮件。")
			return false
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": info.Name}))
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
		return false
	})
	if !found {
		fail(w, 404, "找不到此附件")
	}
}
