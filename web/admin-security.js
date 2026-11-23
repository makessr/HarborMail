"use strict";

function setupAdminSecurity() {
  setupDeploymentSecurity();
  const form = adminElement("admin-security-filters");
  const previous = adminElement("admin-security-previous");
  const next = adminElement("admin-security-next");
  const rows = adminElement("admin-security-items");
  const pageSize = adminElement("admin-security-page-size");
  const pageStatus = adminElement("admin-security-page-status");
  const submit = form.querySelector('button[type="submit"]');
  const date = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "medium" });
  const labels = { success: "登录成功", failed: "登录失败", blocked: "限速拦截", busy: "服务繁忙", error: "会话建立失败" };
  const protocols = { web: "Web", imap: "IMAP", pop3: "POP3", smtp: "SMTP 提交" };
  let filters = new URLSearchParams({ limit: pageSize.value });
  let pages = [""];
  let nextCursor = "";

  for (const [name, all, options] of [["result", "全部结果", labels], ["protocol", "全部协议", protocols]]) {
    form.elements.namedItem(name).replaceChildren(new Option(all, ""), ...Object.entries(options)
      .map(([value, label]) => new Option(label, value)));
  }

  form.addEventListener("submit", event => {
    event.preventDefault();
    if (refresh.disabled) return;
    const params = new URLSearchParams({ limit: pageSize.value });
    for (const [name, input] of new FormData(form)) {
      const value = input.trim();
      if (!value) continue;
      if (name === "from" || name === "until") {
        const time = new Date(value + "T00:00:00");
        if (name === "until") time.setDate(time.getDate() + 1);
        params.set(name, String(time.getTime() / 1000));
      } else {
        params.set(name, value);
      }
    }
    if (params.has("from") && params.has("until") && Number(params.get("from")) >= Number(params.get("until"))) {
      statusOutput.textContent = "结束日期须在起始日期当天或之后。";
      return;
    }
    void loadPage(params, [""]);
  });
  pageSize.addEventListener("change", () => {
    const params = new URLSearchParams(filters);
    params.set("limit", pageSize.value);
    void loadPage(params, [""]);
  });
  previous.addEventListener("click", () => void loadPage(filters, pages.slice(0, -1)));
  next.addEventListener("click", () => void loadPage(filters, [...pages, nextCursor]));

  async function loadPage(query, cursors, includeTotals = false) {
    refresh.disabled = previous.disabled = next.disabled = pageSize.disabled = submit.disabled = true;
    statusOutput.textContent = "正在读取登录记录…";
    const params = new URLSearchParams(query);
    const before = cursors.at(-1);
    if (before) params.set("before", before);
    if (!includeTotals) params.set("totals", "0");
    try {
      const response = await fetch("/api/admin/security?" + params);
      if (response.status === 401 || response.status === 403) {
        location.replace("/");
        return;
      }
      if (!response.ok) throw new Error();
      const data = await response.json();
      adminElement("admin-identity").textContent = data.email;
      adminElement("admin-security-kpis").setAttribute("aria-label", "最近 " + data.retentionDays + " 天登录统计");
      if (data.totals) renderDetails("admin-security-kpis", [
        ["登录成功", data.totals.success || 0],
        ["登录失败", data.totals.failed || 0],
        ["限速拦截", data.totals.blocked || 0],
        ["服务异常", (data.totals.busy || 0) + (data.totals.error || 0)]
      ]);
      renderDetails("admin-security-policy", [
        ["限速窗口", (data.windowSeconds / 60) + " 分钟"],
        ["同一来源", "每窗口 " + data.sourceLimit + " 次"],
        ["同一来源与账号", "每窗口 " + data.pairLimit + " 次"],
        ["可信代理", data.trustedProxies ? data.trustedProxies + " 项" : "直接连接地址"],
        ["限速范围", data.sharedLimit ? "共享 MySQL 的实例" : "当前实例"],
        ["审计保留期", data.retentionDays + " 天"]
      ]);
      rows.replaceChildren(...data.events.map(item => {
        const badge = document.createElement("span");
        badge.className = "admin-security-badge";
        badge.dataset.result = item.result;
        badge.textContent = labels[item.result] || item.result;
        return deliveryRow([date.format(item.at * 1000), item.account || "—", item.source, protocols[item.protocol] || "—", badge]);
      }));
      adminElement("admin-security-empty").hidden = data.events.length > 0;
      filters = query;
      pages = cursors;
      nextCursor = data.hasMore ? data.events.at(-1).id : "";
      pageStatus.textContent = `第 ${pages.length} 页 · 本页 ${data.events.length} 条`;
      activePanel.hidden = false;
      statusOutput.textContent = "";
    } catch {
      statusOutput.textContent = "无法读取登录记录，请刷新重试。";
    } finally {
      pageSize.value = filters.get("limit");
      refresh.disabled = pageSize.disabled = submit.disabled = false;
      previous.disabled = pages.length <= 1;
      next.disabled = !nextCursor;
    }
  }

  return () => loadPage(filters, [""], true);
}

function setupDeploymentSecurity() {
  const run = adminElement("admin-deployment-security-run");
  const status = adminElement("admin-deployment-security-status");
  const results = adminElement("admin-deployment-security-results");
  const labels = { ok: "通过", warning: "需处理", unverified: "待核验" };
  run.addEventListener("click", async () => {
    run.disabled = true;
    status.hidden = false;
    status.textContent = "正在检查…";
    results.hidden = true;
    try {
      const response = await fetch("/api/admin/deployment-security", { cache: "no-store" });
      if (response.status === 401 || response.status === 403) {
        location.replace("/");
        return;
      }
      if (!response.ok) throw new Error();
      const checks = await response.json();
      renderDetails(results.id, checks.map(check => {
        const item = document.createElement("span");
        const badge = document.createElement("span");
        badge.className = "admin-security-badge";
        badge.dataset.result = check.status === "ok" ? "success" : check.status === "warning" ? "error" : "unknown";
        badge.textContent = labels[check.status] || labels.unverified;
        item.append(badge, " " + check.detail);
        return [check.name, item];
      }));
      results.hidden = false;
      status.textContent = "检查完成。";
    } catch {
      status.textContent = "无法完成检查，请重试。";
    } finally {
      run.disabled = false;
    }
  });
}
