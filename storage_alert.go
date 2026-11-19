package main

import (
	"context"
	"errors"
	"net/http"
	"time"
)

const maxStorageThresholdMiB = 1 << 20

type storageAlert struct {
	ThresholdMiB int64  `json:"thresholdMiB"`
	State        string `json:"state"`
}

func storageAlertState(usedBytes, thresholdMiB int64) string {
	if thresholdMiB == 0 {
		return "disabled"
	}
	threshold := thresholdMiB << 20
	switch {
	case usedBytes >= threshold:
		return "reached"
	case usedBytes >= threshold-threshold/5:
		return "approaching"
	default:
		return "normal"
	}
}

func (a *app) adminSetStorageAlert(w http.ResponseWriter, r *http.Request, actor string) {
	a.setStorageLimit(w, r, actor, false)
}

func (a *app) setStorageLimit(w http.ResponseWriter, r *http.Request, actor string, hard bool) {
	target, valid := storageAccountAddress(w, r)
	if !valid {
		return
	}
	var input struct {
		ThresholdMiB *int64 `json:"thresholdMiB"`
		ExpectedMiB  *int64 `json:"expectedMiB"`
		Password     string `json:"password"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !validStorageThreshold(input.ThresholdMiB) || !validStorageThreshold(input.ExpectedMiB) {
		fail(w, http.StatusBadRequest, "容量阈值须为 0 至 1048576 MiB 的整数，0 表示关闭")
		return
	}
	if _, err := a.authenticate(actor, input.Password); err != nil {
		if errors.Is(err, errLoginBusy) {
			fail(w, http.StatusTooManyRequests, "验证请求过多，请稍后重试")
		} else {
			fail(w, http.StatusForbidden, "管理员主邮箱密码不正确")
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if letterError(w, err) {
		return
	}
	defer tx.Rollback()
	if letterError(w, lockIMAPUIDs(ctx, tx)) ||
		browserSessionError(w, a.lockBrowserSession(ctx, tx, actor, browserSessionDigest(r))) {
		return
	}
	if _, err := a.baseAccountHash(ctx, tx, target); letterError(w, err) {
		return
	}
	table, column, action := "mailbox_storage_alerts", "threshold_mib", "storage_alert"
	if hard {
		table, column, action = "mailbox_quota", "limit_mib", "storage_quota"
	}
	var current int64
	err = tx.QueryRowContext(ctx, "SELECT COALESCE((SELECT "+column+" FROM "+table+" WHERE owner=?),0)", target).Scan(&current)
	if letterError(w, err) {
		return
	}
	threshold := *input.ThresholdMiB
	if current == threshold {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if current != *input.ExpectedMiB {
		fail(w, http.StatusConflict, "阈值已被修改，请刷新后重试")
		return
	}
	switch {
	case hard:
		update := " ON CONFLICT(owner) DO UPDATE SET limit_mib=excluded.limit_mib"
		if a.dbDriver == "mysql" {
			update = " ON DUPLICATE KEY UPDATE limit_mib=VALUES(limit_mib)"
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_quota(owner,used_bytes,limit_mib) VALUES(?,0,?)`+update, target, threshold)
	case threshold == 0:
		_, err = tx.ExecContext(ctx, `DELETE FROM mailbox_storage_alerts WHERE owner=?`, target)
	case current == 0:
		_, err = tx.ExecContext(ctx, `INSERT INTO mailbox_storage_alerts(owner,threshold_mib) VALUES(?,?)`, target, threshold)
	default:
		_, err = tx.ExecContext(ctx, `UPDATE mailbox_storage_alerts SET threshold_mib=? WHERE owner=?`, threshold, target)
	}
	if letterError(w, err) {
		return
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO admin_account_actions(actor,target,action,affected,stamp)
		VALUES(?,?,?,?,?)`, actor, target, action, 0, time.Now().Unix())
	if !letterError(w, err) && !letterError(w, tx.Commit()) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func validStorageThreshold(value *int64) bool {
	return value != nil && *value >= 0 && *value <= maxStorageThresholdMiB
}
