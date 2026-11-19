"use strict";

function setupAdminSecurity() {
  const result = adminElement("admin-security-result");
  const search = adminElement("admin-security-search");
  const rows = adminElement("admin-security-items");
  const date = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "medium" });
  const labels = { success: "登录成功", failed: "登录失败", blocked: "限速拦截", busy: "服务繁忙", error: "会话建立失败" };
  const protocols = { web: "Web", imap: "IMAP", pop3: "POP3", smtp: "SMTP 提交" };
  let events = [];

  result.replaceChildren(new Option("全部结果", ""), ...Object.entries(labels)
    .map(([value, label]) => new Option(label, value)));

  function render() {
    const query = search.value.trim().toLowerCase();
    const matching = events.filter(item =>
      (!result.value || item.result === result.value) &&
      [item.account || "", item.source, protocols[item.protocol] || ""].some(value => value.toLowerCase().includes(query)));
    rows.replaceChildren(...matching.map(item => {
      const badge = document.createElement("span");
      badge.className = "admin-security-badge";
      badge.dataset.result = item.result;
      badge.textContent = labels[item.result] || item.result;
      return deliveryRow([date.format(item.at * 1000), item.account || "—", item.source, protocols[item.protocol] || "—", badge]);
    }));
    adminElement("admin-security-empty").hidden = matching.length > 0;
  }

  search.addEventListener("input", render);
  result.addEventListener("change", render);
  return async function loadSecurity() {
    refresh.disabled = true;
    statusOutput.textContent = "正在读取安全记录…";
    try {
      const response = await fetch("/api/admin/security");
      if (response.status === 401 || response.status === 403) {
        location.replace("/");
        return;
      }
      if (!response.ok) throw new Error();
      const data = await response.json();
      adminElement("admin-identity").textContent = data.email;
      renderDetails("admin-security-kpis", [
        ["登录成功", data.totals.success || 0],
        ["登录失败", data.totals.failed || 0],
        ["限速拦截", data.totals.blocked || 0],
        ["服务异常", (data.totals.busy || 0) + (data.totals.error || 0)]
      ]);
      renderDetails("admin-security-policy", [
        ["限速窗口", (data.windowSeconds / 60) + " 分钟"],
        ["同一来源", "每窗口 " + data.sourceLimit + " 次"],
        ["同一来源与账号", "每窗口 " + data.pairLimit + " 次"],
        ["可信代理地址或网段", data.trustedProxies ? data.trustedProxies + " 项" : "未配置，使用连接来源"],
        ["适用范围", "本实例分开计算 Web 与邮件客户端的登录次数"]
      ]);
      events = data.events;
      render();
      activePanel.hidden = false;
      statusOutput.textContent = "";
    } catch {
      statusOutput.textContent = "无法读取安全记录，请刷新重试。";
    } finally {
      refresh.disabled = false;
    }
  };
}
