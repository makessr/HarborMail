"use strict";

async function adminMailRequest(path, options) {
  const response = await fetch("/api/admin/" + path, options);
  if (response.status === 401 || response.status === 403 && !options) {
    location.replace("/");
    throw new Error("请重新登录");
  }
  if (response.status === 204) return null;
  const result = await response.json();
  if (!response.ok) throw new Error(result.error || "暂时无法完成操作");
  return result;
}

function setupAdminDeliveryTools() {
  const filters = adminElement("admin-delivery-log-form");
  const state = adminElement("admin-delivery-log-state");
  const rows = adminElement("admin-delivery-log-items");
  const more = adminElement("admin-delivery-log-more");
  const status = adminElement("admin-delivery-log-status");
  const limitForm = adminElement("admin-outbound-limit-form");
  const limitRows = adminElement("admin-limit-items");
  const limitStatus = adminElement("admin-limit-status");
  const labels = { ...queueLabels, enqueued: "入队", sent: "投递成功", accepted: "对端已接收",
    local_delivered: "站内已送达", retry_requested: "申请重试", cancelled: "已取消" };
  state.append(...Object.entries(labels).map(([value, label]) => new Option(label, value)));
  let cursor = 0;
  let query = new URLSearchParams();
  let busy = false;
  async function loadLogs(reset) {
    if (busy) return;
    busy = more.disabled = true;
    status.textContent = "正在读取投递日志…";
    if (reset) {
      query = new URLSearchParams(new FormData(filters));
      cursor = 0;
    }
    const params = new URLSearchParams(query);
    if (cursor) params.set("before", cursor);
    try {
      const { events, next } = await adminMailRequest("delivery-logs?" + params);
      if (reset) rows.replaceChildren();
      rows.append(...events.map(item => deliveryRow([
        deliveryDate.format(item.at * 1000), "#" + item.outbox, item.owner, item.address || "—",
        labels[item.state] || item.state, item.code || "—"
      ])));
      cursor = next;
      more.hidden = !next;
      status.textContent = !rows.children.length ? "暂无匹配的投递事件。" : "";
    } catch (error) {
      status.textContent = error.message;
    } finally {
      busy = more.disabled = false;
    }
  }
  async function loadLimits() {
    const limits = await adminMailRequest("outbound-limits");
    limitRows.replaceChildren(...limits.map(item => deliveryRow([
      item.scope === "account" ? "邮箱账号" : "邮件域名", item.target, item.used, item.limit
    ])));
    limitStatus.textContent = limits.length ? "" : "暂无外发额度。";
  }
  filters.addEventListener("submit", event => {
    event.preventDefault();
    void loadLogs(true);
  });
  more.addEventListener("click", () => void loadLogs(false));
  limitForm.addEventListener("submit", async event => {
    event.preventDefault();
    const button = limitForm.querySelector("button[type=submit]");
    if (button.disabled) return;
    button.disabled = true;
    const data = Object.fromEntries(new FormData(limitForm));
    data.limit = Number(data.limit);
    limitForm.elements.password.value = "";
    try {
      await adminMailRequest("outbound-limits", {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data)
      });
      await loadLimits();
      limitStatus.textContent = data.limit ? "额度已保存。" : "额度已移除。";
    } catch (error) {
      limitStatus.textContent = error.message;
    } finally {
      button.disabled = false;
    }
  });
  return () => {
    activePanel.hidden = false;
    void loadLogs(true);
    void loadLimits().catch(error => { limitStatus.textContent = error.message; });
  };
}

function setupAdminDMARC() {
  const filter = adminElement("admin-dmarc-filter");
  const upload = adminElement("admin-dmarc-upload");
  const status = adminElement("admin-dmarc-status");
  const rows = adminElement("admin-dmarc-items");
  const detail = adminElement("admin-dmarc-sources");
  const detailRows = adminElement("admin-dmarc-source-items");
  const utc = new Intl.DateTimeFormat("zh-CN", { dateStyle: "short", timeStyle: "short", timeZone: "UTC" });
  let detailRequest = 0;
  async function showSources(report) {
    const request = ++detailRequest;
    detail.hidden = true;
    try {
      const sources = await adminMailRequest("dmarc-reports?" + new URLSearchParams({ key: report.key }));
      if (request !== detailRequest) return;
      adminElement("admin-dmarc-sources-title").textContent = report.domain + " 的发信来源";
      const alignment = value => value === "pass" ? "通过" : "失败";
      const disposition = { none: "无", quarantine: "隔离", reject: "拒收" };
      detailRows.replaceChildren(...sources.map(item => deliveryRow([
        item.address, item.count, alignment(item.dkim), alignment(item.spf), disposition[item.disposition]
      ])));
      detail.hidden = false;
    } catch (error) {
      if (request === detailRequest) status.textContent = error.message;
    }
  }
  async function load() {
    ++detailRequest;
    detail.hidden = true;
    try {
      const reports = await adminMailRequest("dmarc-reports?" + new URLSearchParams(new FormData(filter)));
      rows.replaceChildren(...reports.map(report => {
        const button = document.createElement("button");
        button.className = "secondary";
        button.type = "button";
        button.textContent = "查看来源";
        button.addEventListener("click", () => void showSources(report));
        return deliveryRow([
          report.domain, report.reporter, utc.formatRange(report.start * 1000, report.end * 1000),
          report.total, report.aligned + " / " + report.total, button
        ]);
      }));
      status.textContent = reports.length ? "" : "暂无报告。可导入收件服务器发送的聚合报告附件。";
    } catch (error) {
      status.textContent = error.message;
    }
  }
  filter.addEventListener("submit", event => {
    event.preventDefault();
    void load();
  });
  upload.addEventListener("submit", async event => {
    event.preventDefault();
    const file = upload.elements.report.files[0];
    const button = upload.querySelector("button[type=submit]");
    if (!file || button.disabled) return;
    button.disabled = true;
    try {
      await adminMailRequest("dmarc-reports", { method: "POST", body: file });
      upload.reset();
      await load();
      if (rows.children.length) status.textContent = "报告已导入。";
    } catch (error) {
      status.textContent = error.message;
    } finally {
      button.disabled = false;
    }
  });
  return () => {
    activePanel.hidden = false;
    void load();
  };
}
