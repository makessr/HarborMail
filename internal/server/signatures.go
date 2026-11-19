package server

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxSignatures = 20
const maxSignatureBytes = 4096

type mailSignature struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Body string `json:"body"`
}

type signatureDefaults struct {
	Compose string `json:"compose"`
	Reply   string `json:"reply"`
	Forward string `json:"forward"`
}

type signatureOptions struct {
	Signatures []mailSignature   `json:"signatures"`
	Defaults   signatureDefaults `json:"defaults"`
}

type signatureSettings struct {
	Revision int64 `json:"revision"`
	signatureOptions
}

func (s *signatureSettings) normalize() string {
	if s.Revision < 0 {
		return "签名设置版本无效"
	}
	if len(s.Signatures) > maxSignatures {
		return "每个邮箱最多可保存 20 个签名"
	}
	if s.Signatures == nil {
		s.Signatures = []mailSignature{}
	}
	ids := make(map[string]struct{}, len(s.Signatures))
	names := make(map[string]struct{}, len(s.Signatures))
	for i := range s.Signatures {
		item := &s.Signatures[i]
		item.Name = strings.TrimSpace(item.Name)
		item.Body = strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(item.Body, "\r\n", "\n"), "\r", "\n"), "\n")
		if _, err := hex.DecodeString(item.ID); err != nil || len(item.ID) != 32 || item.ID != strings.ToLower(item.ID) {
			return "签名编号无效，请重新打开签名设置"
		}
		if _, exists := ids[item.ID]; exists {
			return "签名编号重复，请重新打开签名设置"
		}
		if item.Name == "" || utf8.RuneCountInString(item.Name) > 40 || strings.ContainsFunc(item.Name, unicode.IsControl) {
			return "签名名称须为 1–40 个字符"
		}
		nameKey := strings.ToLower(item.Name)
		if _, exists := names[nameKey]; exists {
			return "签名名称不能重复"
		}
		if strings.TrimSpace(item.Body) == "" || len(item.Body) > maxSignatureBytes || strings.ContainsFunc(item.Body, func(r rune) bool {
			return unicode.IsControl(r) && r != '\n' && r != '\t'
		}) {
			return "请输入有效的签名文本，最多 4 KiB"
		}
		ids[item.ID] = struct{}{}
		names[nameKey] = struct{}{}
	}
	for _, id := range []string{s.Defaults.Compose, s.Defaults.Reply, s.Defaults.Forward} {
		if _, exists := ids[id]; id != "" && !exists {
			return "请选择有效的默认签名"
		}
	}
	return ""
}

func (a *app) readSignatures(ctx context.Context, owner string) (signatureSettings, error) {
	var settings signatureSettings
	settings.Signatures = []mailSignature{}
	var value []byte
	err := a.db.QueryRowContext(ctx, `SELECT revision,settings FROM signature_settings WHERE owner=?`, owner).Scan(&settings.Revision, &value)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	return settings, json.Unmarshal(value, &settings.signatureOptions)
}

func signatureHTTPError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, sql.ErrNoRows) || uniqueConflict(err):
		fail(w, http.StatusConflict, "签名设置已在其他窗口修改，请关闭后重新打开再编辑")
	default:
		log.Print("signature settings: ", err)
		fail(w, http.StatusInternalServerError, "无法读取或保存签名设置")
	}
	return true
}

func (a *app) getSignatures(w http.ResponseWriter, r *http.Request, owner string) {
	settings, err := a.readSignatures(r.Context(), owner)
	if !signatureHTTPError(w, err) {
		w.Header().Set("Cache-Control", "no-store")
		reply(w, http.StatusOK, settings)
	}
}

func (a *app) putSignatures(w http.ResponseWriter, r *http.Request, owner string) {
	var settings signatureSettings
	if !decode(w, r, &settings) {
		return
	}
	if invalid := settings.normalize(); invalid != "" {
		fail(w, http.StatusBadRequest, invalid)
		return
	}
	value, err := json.Marshal(settings.signatureOptions)
	if signatureHTTPError(w, err) {
		return
	}
	ctx := r.Context()
	if settings.Revision == 0 {
		_, err = a.db.ExecContext(ctx, `INSERT INTO signature_settings(owner,revision,settings) VALUES(?,1,?)`, owner, value)
	} else {
		result, updateErr := a.db.ExecContext(ctx, `UPDATE signature_settings SET revision=revision+1,settings=? WHERE owner=? AND revision=?`,
			value, owner, settings.Revision)
		err = requireAffectedRow(result, updateErr)
	}
	if !signatureHTTPError(w, err) {
		settings.Revision++
		reply(w, http.StatusOK, settings)
	}
}
