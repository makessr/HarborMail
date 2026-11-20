"use strict";

function setupAdminAudit() {
  const form = adminElement("admin-audit-filter");
  const action = adminElement("admin-audit-action");
  const apply = adminElement("admin-audit-apply");
  const exportButton = adminElement("admin-audit-export");
  const body = adminElement("admin-audit-events");
  const status = adminElement("admin-audit-status");
  const more = adminElement("admin-audit-more");
  const labels = { ...accessLabels, retry: "提前重试投递", alias_add: "添加收信别名", alias_remove: "移除收信别名",
    domain_add: "新增域名", domain_remove: "移除域名", dkim_generate: "生成 DKIM 密钥", dkim_enable: "启用 DKIM 签名", dkim_disable: "停用 DKIM 签名",
    delivery_confirmed: "确认中继已接收", delivery_rejected: "确认中继未接收",
    recipient_confirmed: "确认单地址已接收", recipient_rejected: "确认单地址未接收" };
  const timestamp = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "medium" });
  const formatTime = value => timestamp.format(value * 1000);
  let filters;
  let next = "";
  let busy = false;

  action.replaceChildren(new Option("全部操作", ""), ...Object.entries(labels)
    .map(([value, label]) => new Option(label, value)));
  const initial = new URLSearchParams(location.search);
  if (Object.hasOwn(labels, initial.get("action"))) action.value = initial.get("action");
  for (const key of ["actor", "target"]) form.elements[key].value = (initial.get(key) || "").slice(0, 254);
  filters = readAdminFilters(form);

  function setBusy(value) {
    busy = value;
    apply.disabled = exportButton.disabled = more.disabled = refresh.disabled = value;
  }

  async function requestAudit(url) {
    const response = await fetch(url);
    if (response.status === 401 || response.status === 403) {
      location.replace("/");
      return null;
    }
    if (!response.ok) {
      const error = await response.json().catch(() => null);
      throw new Error(error?.error || "无法读取审计数据，请稍后重试。");
    }
    return response;
  }

  async function load(before = "") {
    if (busy) return;
    setBusy(true);
    statusOutput.textContent = "正在读取审计记录…";
    try {
      const query = new URLSearchParams(filters);
      if (before) query.set("before", before);
      const response = await requestAudit("/api/admin/audit?" + query);
      if (!response) return;
      const data = await response.json();
      adminElement("admin-identity").textContent = data.email;
      if (!before) body.replaceChildren();
      body.append(...data.events.map(event => {
        const detail = event.alias || (event.action === "retry" ? "任务 #" + event.jobId + "\n原重试时间 " + formatTime(event.previousDue) :
          event.jobId ? "任务 #" + event.jobId :
          ["sessions", "app_passwords", "suspend", "password_reset"].includes(event.action) ? "撤销 " + event.affected + " 条记录" : "—");
        return deliveryRow([formatTime(event.stamp), labels[event.action] || event.action, event.actor, event.target, detail]);
      }));
      next = data.next;
      more.hidden = !next;
      adminElement("admin-audit-empty").hidden = body.childElementCount !== 0;
      statusOutput.textContent = "";
      activePanel.hidden = false;
    } catch (error) {
      statusOutput.textContent = error.message;
    } finally {
      setBusy(false);
    }
  }

  form.addEventListener("submit", event => {
    event.preventDefault();
    if (busy) return;
    filters = readAdminFilters(form);
    next = "";
    more.hidden = true;
    body.replaceChildren();
    adminElement("admin-audit-empty").hidden = true;
    status.textContent = "";
    void load();
  });
  more.addEventListener("click", () => { if (next) void load(next); });
  exportButton.addEventListener("click", async () => {
    if (busy) return;
    setBusy(true);
    status.textContent = "正在生成 CSV…";
    try {
      const response = await requestAudit("/api/admin/audit/export?" + filters);
      if (!response) return;
      const url = URL.createObjectURL(await response.blob());
      const link = document.createElement("a");
      link.href = url;
      link.download = "harbor-mail-audit.csv";
      document.body.append(link);
      link.click();
      link.remove();
      setTimeout(() => URL.revokeObjectURL(url), 3000);
      status.textContent = "已生成 CSV。";
    } catch (error) {
      status.textContent = error.message;
    } finally {
      setBusy(false);
    }
  });
  return () => load();
}
