package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
)

type storageBucket struct {
	Key   string `json:"key"`
	Label string `json:"label,omitempty"`
	Count int64  `json:"count"`
	Bytes int64  `json:"bytes"`
}

type accountStorage struct {
	Email        string          `json:"email"`
	At           int64           `json:"at"`
	MessageCount int64           `json:"messageCount"`
	MessageBytes int64           `json:"messageBytes"`
	QueueCount   int64           `json:"queueCount"`
	QueueBytes   int64           `json:"queueBytes"`
	Folders      []storageBucket `json:"folders"`
	Queue        []storageBucket `json:"queue"`
	Alert        storageAlert    `json:"alert"`
	Quota        *storageQuota   `json:"quota,omitempty"`
}

type storageQuota struct {
	LimitMiB  int64 `json:"limitMiB"`
	UsedBytes int64 `json:"usedBytes"`
}

// Keep on-demand scans from piling up on the mailbox database.
var storageQuerySlot = make(chan struct{}, 1)

func (a *app) adminAccountStorage(w http.ResponseWriter, r *http.Request, _ string) {
	target, valid := storageAccountAddress(w, r)
	if !valid {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if _, err := a.baseAccountHash(ctx, a.db, target); errors.Is(err, sql.ErrNoRows) {
		fail(w, http.StatusNotFound, "未找到该主邮箱")
		return
	} else if err != nil {
		fail(w, http.StatusServiceUnavailable, "无法读取邮箱账号，请稍后刷新")
		return
	}
	key := storageCacheKey(target)
	var cached accountStorage
	hit, reachable := a.adminCache.read(ctx, key, &cached)
	if hit && cached.Email == target && cached.At > 0 && cached.Folders != nil && cached.Queue != nil {
		if err := a.loadLiveStorageSettings(ctx, &cached); err == nil {
			reply(w, http.StatusOK, cached)
			return
		}
	}
	select {
	case storageQuerySlot <- struct{}{}:
		defer func() { <-storageQuerySlot }()
	default:
		fail(w, http.StatusTooManyRequests, "已有存储统计在运行，请稍后刷新")
		return
	}
	usage, err := a.readAccountStorage(ctx, target)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "无法在统计时间内读取存储用量，请稍后刷新")
		return
	}
	if reachable {
		snapshot := usage
		snapshot.Quota, snapshot.Alert = nil, storageAlert{}
		a.adminCache.write(ctx, key, snapshot, storageCacheTTL)
	}
	reply(w, http.StatusOK, usage)
}

func (a *app) loadLiveStorageSettings(ctx context.Context, usage *accountStorage) error {
	var quota storageQuota
	err := a.db.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT threshold_mib FROM mailbox_storage_alerts WHERE owner=?),0),
		COALESCE(q.limit_mib,0),COALESCE(q.used_bytes,0)
		FROM (SELECT ? AS owner) mailbox LEFT JOIN mailbox_quota q ON q.owner=mailbox.owner`,
		usage.Email, usage.Email).Scan(&usage.Alert.ThresholdMiB, &quota.LimitMiB, &quota.UsedBytes)
	if err != nil {
		return err
	}
	usage.Quota = nil
	if a.hardQuota {
		usage.Quota = &quota
	}
	usage.Alert.State = storageAlertState(usage.MessageBytes+usage.QueueBytes, usage.Alert.ThresholdMiB)
	return nil
}

func (a *app) readAccountStorage(ctx context.Context, owner string) (accountStorage, error) {
	result := accountStorage{
		Email: owner, At: time.Now().Unix(),
		Folders: make([]storageBucket, 0), Queue: make([]storageBucket, 0),
	}
	if a.hardQuota {
		result.Quota = &storageQuota{}
	}
	// Both drivers return bytes from LENGTH on raw BLOBs. A single statement
	// keeps folder and queue totals on the same snapshot during delivery.
	rows, err := a.db.QueryContext(ctx, `SELECT 0, grouped.folder, COALESCE(f.label,''), grouped.item_count, grouped.byte_count
		FROM (SELECT folder,COUNT(*) AS item_count,SUM(LENGTH(raw)) AS byte_count
			FROM letters WHERE owner=? GROUP BY folder) grouped
		LEFT JOIN custom_folders f ON f.folder=grouped.folder AND f.owner=?
		UNION ALL
		SELECT 1,state,'',COUNT(*),SUM(LENGTH(raw))
		FROM outbox WHERE sender=? GROUP BY state
		UNION ALL
		SELECT 2,'','',0,threshold_mib FROM mailbox_storage_alerts WHERE owner=?
		UNION ALL
		SELECT 3,'','',used_bytes,limit_mib FROM mailbox_quota WHERE owner=?
		ORDER BY 1,5 DESC,2`, owner, owner, owner, owner, owner)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind int
		var bucket storageBucket
		if err := rows.Scan(&kind, &bucket.Key, &bucket.Label, &bucket.Count, &bucket.Bytes); err != nil {
			return result, err
		}
		switch kind {
		case 1:
			result.Queue = append(result.Queue, bucket)
			result.QueueCount += bucket.Count
			result.QueueBytes += bucket.Bytes
		case 2:
			result.Alert.ThresholdMiB = bucket.Bytes
		case 3:
			if result.Quota != nil {
				*result.Quota = storageQuota{LimitMiB: bucket.Bytes, UsedBytes: bucket.Count}
			}
		default:
			result.Folders = append(result.Folders, bucket)
			result.MessageCount += bucket.Count
			result.MessageBytes += bucket.Bytes
		}
	}
	result.Alert.State = storageAlertState(result.MessageBytes+result.QueueBytes, result.Alert.ThresholdMiB)
	return result, rows.Err()
}

func storageAccountAddress(w http.ResponseWriter, r *http.Request) (string, bool) {
	target := strings.ToLower(strings.TrimSpace(r.PathValue("email")))
	if canonical, err := address(target); err != nil || canonical != target {
		fail(w, http.StatusBadRequest, "请选择有效的主邮箱")
		return "", false
	}
	return target, true
}
