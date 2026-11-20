package server

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

var browserTranslationSlots = make(chan struct{}, 4)

type browserTranslationInput struct {
	SourceVersion string          `json:"sourceVersion"`
	Translation   translatedEmail `json:"translation"`
}

type browserTranslationSource struct {
	SourceVersion string           `json:"sourceVersion,omitempty"`
	Original      *translatedEmail `json:"original,omitempty"`
	Cached        *translatedEmail `json:"cached,omitempty"`
}

func (a *app) browserTranslateLetter(w http.ResponseWriter, r *http.Request, owner string) {
	if a.recoveryMode {
		fail(w, http.StatusServiceUnavailable, "恢复模式下无法翻译邮件。")
		return
	}
	source, target := r.URL.Query().Get("source"), r.URL.Query().Get("target")
	if !validTranslationLanguage(source) || !validTranslationLanguage(target) || source == target {
		fail(w, http.StatusBadRequest, "请选择不同的原文语言和目标语言。")
		return
	}
	var input browserTranslationInput
	if r.Method == http.MethodPost {
		if !decode(w, r, &input) {
			return
		}
		if strings.TrimSpace(input.Translation.Subject) == "" && strings.TrimSpace(input.Translation.Body) == "" {
			fail(w, http.StatusBadRequest, "译文为空。")
			return
		}
		if len(input.Translation.Subject)+len(input.Translation.Body) > maxTranslationCacheEntry {
			fail(w, http.StatusRequestEntityTooLarge, "译文超出缓存大小。")
			return
		}
	}

	select {
	case browserTranslationSlots <- struct{}{}:
		defer func() { <-browserTranslationSlots }()
	default:
		fail(w, http.StatusTooManyRequests, "翻译服务繁忙，请稍后再试。")
		return
	}
	id := letterID(r)
	var subject string
	var raw []byte
	err := a.db.QueryRowContext(r.Context(), `SELECT subject,raw FROM letters WHERE owner=? AND id=? AND folder NOT IN ('drafts','outbox')`, owner, id).Scan(&subject, &raw)
	if letterError(w, err) {
		return
	}
	key := mailTranslationKey(owner, id, "browser:"+source, target, subject, raw)
	version := hex.EncodeToString(key.content[:])
	if r.Method == http.MethodGet {
		if cached, ok := a.cachedTranslation(r.Context(), key); ok {
			reply(w, http.StatusOK, browserTranslationSource{Cached: &cached})
			return
		}
		original, ok := translationSource(w, subject, raw)
		if ok {
			reply(w, http.StatusOK, browserTranslationSource{SourceVersion: version, Original: &original})
		}
		return
	}

	if input.SourceVersion != version {
		fail(w, http.StatusConflict, "邮件内容已更新，请重新读取原文。")
		return
	}
	if err := a.storeTranslation(r.Context(), key, subject, raw, input.Translation); errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusConflict, "邮件已更新或移除，请重新读取原文。")
		return
	} else if err != nil {
		fail(w, http.StatusServiceUnavailable, "暂时无法缓存译文。")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
