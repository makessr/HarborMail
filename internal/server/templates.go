package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxMailTemplates = 20
const maxTemplateBytes = 4096

type mailTemplate struct {
	Name    string `json:"name"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

type mailTemplates struct {
	Revision  int64          `json:"revision"`
	Templates []mailTemplate `json:"templates"`
}

func (settings *mailTemplates) normalize() string {
	if settings.Revision < 0 || settings.Templates == nil {
		return "模板设置格式无效，请重新打开邮件模板"
	}
	if len(settings.Templates) > maxMailTemplates {
		return "每个邮箱最多可保存 20 份邮件模板"
	}
	names := make(map[string]struct{}, len(settings.Templates))
	for i := range settings.Templates {
		item := &settings.Templates[i]
		item.Name = strings.TrimSpace(item.Name)
		if item.Name == "" || utf8.RuneCountInString(item.Name) > 40 || strings.ContainsFunc(item.Name, unicode.IsControl) {
			return "模板名称须为 1–40 个字符"
		}
		nameKey := strings.ToLower(item.Name)
		if _, exists := names[nameKey]; exists {
			return "模板名称不能重复"
		}
		names[nameKey] = struct{}{}
		if len(item.Subject) > 300 || strings.ContainsFunc(item.Subject, unicode.IsControl) {
			return "模板主题须为单行文本，最多 300 字节"
		}
		item.Subject = strings.TrimSpace(item.Subject)
		item.Body = strings.ReplaceAll(strings.ReplaceAll(item.Body, "\r\n", "\n"), "\r", "\n")
		if strings.TrimSpace(item.Body) == "" || len(item.Body) > maxTemplateBytes || strings.ContainsFunc(item.Body, func(r rune) bool {
			return unicode.IsControl(r) && r != '\n' && r != '\t'
		}) {
			return "请输入有效的模板正文，最多 4 KiB"
		}
	}
	return ""
}

func mailTemplateError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sql.ErrNoRows) || uniqueConflict(err) {
		fail(w, http.StatusConflict, "模板已在其他窗口修改，请重新打开后再编辑")
	} else {
		log.Print("mail templates: ", err)
		fail(w, http.StatusInternalServerError, "无法读取或保存邮件模板")
	}
	return true
}

func (a *app) getMailTemplates(w http.ResponseWriter, r *http.Request, owner string) {
	settings := mailTemplates{Templates: []mailTemplate{}}
	var content []byte
	err := a.db.QueryRowContext(r.Context(), `SELECT revision,templates FROM mail_templates WHERE owner=?`, owner).Scan(&settings.Revision, &content)
	if !errors.Is(err, sql.ErrNoRows) {
		if mailTemplateError(w, err) || mailTemplateError(w, json.Unmarshal(content, &settings.Templates)) {
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	reply(w, http.StatusOK, settings)
}

func (a *app) putMailTemplates(w http.ResponseWriter, r *http.Request, owner string) {
	var settings mailTemplates
	if !decode(w, r, &settings) {
		return
	}
	if invalid := settings.normalize(); invalid != "" {
		fail(w, http.StatusBadRequest, invalid)
		return
	}
	content, err := json.Marshal(settings.Templates)
	if mailTemplateError(w, err) {
		return
	}
	ctx := r.Context()
	if settings.Revision == 0 {
		_, err = a.db.ExecContext(ctx, `INSERT INTO mail_templates(owner,revision,templates) VALUES(?,1,?)`, owner, content)
	} else {
		result, updateErr := a.db.ExecContext(ctx, `UPDATE mail_templates SET revision=revision+1,templates=? WHERE owner=? AND revision=?`,
			content, owner, settings.Revision)
		err = requireAffectedRow(result, updateErr)
	}
	if !mailTemplateError(w, err) {
		settings.Revision++
		reply(w, http.StatusOK, settings)
	}
}
