package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type deploymentCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func (a *app) adminDeploymentSecurity(w http.ResponseWriter, r *http.Request, actor string) {
	w.Header().Set("Cache-Control", "no-store")
	https := deploymentCheck{"HTTPS", "unverified", "应用未直接终止 TLS。请从公网 HTTPS 入口核验反向代理和证书。"}
	if r.TLS != nil {
		https = deploymentCheck{"HTTPS", "ok", "本次请求由 Harbor Mail 直接使用 TLS 接收。"}
	}
	cookie := deploymentCheck{"会话 Cookie", "warning", "未开启 Secure。HTTPS 反向代理后请配置 HARBOR_SECURE_COOKIE=true。"}
	if a.secure {
		cookie = deploymentCheck{"会话 Cookie", "ok", "服务器会给登录 Cookie 设置 Secure 属性。"}
	}
	proxy := deploymentCheck{"可信代理", "unverified", "未配置。直连部署无需配置，使用反向代理时请核对访问日志中的来源地址。"}
	if len(a.webLogin.proxies) > 0 {
		proxy.Detail = "已配置可信代理。请核对代理覆盖转发头，且信任范围只包含代理地址。"
	}
	mfa := deploymentCheck{"当前管理员的部署托管两步验证", "warning", "当前管理员未使用服务器配置的两步验证。"}
	if _, ok := a.adminMFA[actor]; ok {
		mfa = deploymentCheck{"当前管理员的部署托管两步验证", "ok", "当前管理员已启用服务器配置的两步验证。"}
	}
	reply(w, http.StatusOK, []deploymentCheck{https, cookie, proxy, mfa, a.deploymentScanner(r.Context()),
		deploymentBackup(os.Getenv("HARBOR_BACKUP_STATUS_FILE"), time.Now())})
}

func (a *app) deploymentScanner(ctx context.Context) deploymentCheck {
	result := deploymentCheck{"来信扫描", "unverified", "未配置来信扫描。"}
	if a.spamScanner == nil {
		return result
	}
	if !a.inbound.beginScan(false) {
		result.Detail = "扫描器当前繁忙，请稍后重查。"
		return result
	}
	defer a.inbound.endScan()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	verdict, err := a.spamScanner.check(ctx, &delivery{sender: "probe@harbor.test"},
		[]byte("From: probe@harbor.test\r\nTo: probe@harbor.test\r\nSubject: Harbor scan health\r\n\r\nSynthetic health check.\r\n"))
	if err != nil || verdict.unavailable || verdict.virus {
		return deploymentCheck{"来信扫描", "warning", "合成扫描未成功。请检查 Rspamd、ClamAV 和扫描超时。"}
	}
	if !a.scanPolicy.deferUnavailable {
		return deploymentCheck{"来信扫描", "warning", "扫描接口已响应。扫描故障时当前配置仍允许投递。"}
	}
	return deploymentCheck{"来信扫描", "ok", "扫描接口已响应。扫描故障时会暂缓投递。"}
}

func deploymentBackup(path string, now time.Time) deploymentCheck {
	result := deploymentCheck{"备份回读", "unverified", "未接入备份成功时间。请核对最近一次异地备份与恢复演练。"}
	if path == "" {
		return result
	}
	stamp, err := readDeploymentBackup(path)
	if err != nil || stamp > now.Add(5*time.Minute).Unix() {
		return deploymentCheck{"备份回读", "warning", "备份成功记录无法读取或时间无效。请检查只读挂载和备份任务。"}
	}
	if now.Unix()-stamp > int64((26*time.Hour)/time.Second) {
		return deploymentCheck{"备份回读", "warning", "最近一次成功回读超过 26 小时。请检查备份任务。"}
	}
	return deploymentCheck{"备份回读", "ok", "最近 26 小时内记录了成功的备份回读。"}
}

func readDeploymentBackup(path string) (int64, error) {
	const marker = "harbor_backup_last_success_timestamp_seconds "
	invalid := errors.New("invalid backup status file")
	if !filepath.IsAbs(path) {
		return 0, invalid
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 512 {
		return 0, invalid
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 513))
	if err != nil || len(raw) > 512 {
		return 0, invalid
	}
	value, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), marker)
	stamp, err := strconv.ParseInt(value, 10, 64)
	if !ok || err != nil || stamp <= 0 {
		return 0, invalid
	}
	return stamp, nil
}
