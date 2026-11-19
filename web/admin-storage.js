"use strict";

const storageFolderNames = {
  inbox: "收件箱", sent: "已发送", drafts: "草稿箱", archive: "归档",
  junk: "垃圾邮件", trash: "废纸篓", snoozed: "稍后处理"
};
const storageNumber = new Intl.NumberFormat("zh-CN");
const storageDecimal = new Intl.NumberFormat("zh-CN", { maximumFractionDigits: 1 });
const storagePercent = new Intl.NumberFormat("zh-CN", { style: "percent", maximumFractionDigits: 1 });

function storageSize(bytes) {
  let value = bytes;
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB"];
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return storageDecimal.format(value) + " " + units[unit];
}

function renderStorageBuckets(container, buckets, labels, total, queue = false) {
  if (!buckets.length) {
    const empty = document.createElement("p");
    empty.className = "admin-note";
    empty.textContent = queue ? "该账号没有投递任务。" : "该账号没有存储邮件。";
    container.replaceChildren(empty);
    return;
  }
  const table = document.createElement("table");
  const header = table.createTHead().insertRow();
  for (const heading of [queue ? "状态" : "文件夹", "件数", "占用", "占比"]) {
    const cell = document.createElement("th");
    cell.scope = "col";
    cell.textContent = heading;
    header.append(cell);
  }
  const body = table.createTBody();
  for (const { key, label, bytes, count } of buckets) {
    const row = body.insertRow();
    row.insertCell().textContent = label || (Object.hasOwn(labels, key) ? labels[key] : key);
    row.insertCell().textContent = storageNumber.format(count);
    const size = row.insertCell();
    size.textContent = storageSize(bytes);
    size.title = storageNumber.format(bytes) + " 字节";
    const share = total ? bytes / total : 0;
    const cell = row.insertCell();
    cell.className = "admin-storage-share";
    const meter = document.createElement("meter");
    meter.min = 0;
    meter.max = 1;
    meter.value = share;
    meter.setAttribute("aria-hidden", "true");
    const text = document.createElement("span");
    text.textContent = storagePercent.format(share);
    cell.append(meter, text);
  }
  const wrapper = document.createElement("div");
  wrapper.className = "admin-table admin-storage-table";
  wrapper.append(table);
  container.replaceChildren(wrapper);
}

function setupAdminStorage() {
  const form = adminElement("admin-storage-form");
  const owner = adminElement("admin-storage-owner");
  const submit = form.querySelector("button");
  const result = adminElement("admin-storage-result");
  const hint = adminElement("admin-storage-hint");
  const alertEditor = adminElement("admin-storage-alert-dialog");
  const alertForm = adminElement("admin-storage-alert-form");
  const alertClose = adminElement("admin-storage-alert-close");
  const alertSave = adminElement("admin-storage-alert-save");
  const alertError = adminElement("admin-storage-alert-error");
  const alertEdit = adminElement("admin-storage-alert-edit");
  let currentAlert;
  let currentQuota;
  let editingQuota = false;
  let busy = false;

  function renderAlert(data) {
    currentAlert = data.alert;
    currentQuota = data.quota;
    adminElement("admin-storage-quota").hidden = !currentQuota;
    if (currentQuota) {
      adminElement("admin-storage-quota-label").textContent = currentQuota.limitMiB
        ? storageSize(currentQuota.usedBytes) + " / " + storageSize(currentQuota.limitMiB * 1024 ** 2)
        : "未限制存储";
    }
    const label = adminElement("admin-storage-alert-label");
    label.textContent = {
      disabled: "未设置阈值", normal: "用量正常",
      approaching: "接近阈值", reached: "已达到阈值"
    }[data.alert.state];
    const usage = adminElement("admin-storage-alert-usage");
    usage.hidden = data.alert.thresholdMiB === 0;
    if (usage.hidden) return;
    const used = data.messageBytes + data.queueBytes;
    const threshold = data.alert.thresholdMiB * 1024 ** 2;
    const share = used / threshold;
    const meter = adminElement("admin-storage-alert-meter");
    meter.value = Math.min(100, share * 100);
    meter.dataset.state = data.alert.state;
    adminElement("admin-storage-alert-bytes").textContent =
      storageSize(used) + " / " + storageSize(threshold) + " · " + storagePercent.format(share);
  }

  async function request(path) {
    const response = await fetch(path, { cache: "no-store" });
    if (response.status === 401 || response.status === 403) {
      location.replace("/");
      return null;
    }
    const data = await response.json().catch(() => null);
    if (!response.ok || !data) throw new Error(data?.error || "无法读取存储用量，请刷新重试。");
    return data;
  }

  async function load(refreshDirectory) {
    if (busy) return;
    busy = refresh.disabled = owner.disabled = submit.disabled = true;
    result.hidden = hint.hidden = true;
    statusOutput.textContent = refreshDirectory ? "正在读取邮箱账号…" : "正在统计存储用量…";
    try {
      const url = new URL(location.href);
      if (refreshDirectory) {
        const data = await request("/api/admin/accounts");
        if (!data) return;
        adminElement("admin-identity").textContent = data.email;
        const names = data.accounts.map(({ email }) => email);
        const target = owner.value || url.searchParams.get("owner");
        owner.replaceChildren(new Option("请选择主邮箱", ""), ...names.map(name => new Option(name, name)));
        if (names.includes(target)) owner.value = target;
        activePanel.hidden = false;
      }
      if (!owner.value) {
        hint.hidden = false;
        statusOutput.textContent = "";
        return;
      }
      url.searchParams.set("owner", owner.value);
      history.replaceState(null, "", url);
      statusOutput.textContent = "正在统计存储用量…";
      const data = await request("/api/admin/accounts/" + encodeURIComponent(owner.value) + "/storage");
      if (!data) return;
      adminElement("admin-storage-stamp").textContent = "统计于 " + queueTime(data.at);
      renderDetails("admin-storage-summary", [
        ["邮件占用", storageSize(data.messageBytes)],
        ["邮件数量", storageNumber.format(data.messageCount)],
        ["队列占用", storageSize(data.queueBytes)],
        ["队列任务", storageNumber.format(data.queueCount)]
      ]);
      renderStorageBuckets(adminElement("admin-storage-folders"), data.folders, storageFolderNames, data.messageBytes);
      renderStorageBuckets(adminElement("admin-storage-queue"), data.queue, queueLabels, data.queueBytes, true);
      adminElement("admin-storage-queue-link").href = "/admin/queue?" + new URLSearchParams({ sender: data.email });
      renderAlert(data);
      result.hidden = false;
      statusOutput.textContent = "";
    } catch (error) {
      statusOutput.textContent = error.message || "无法读取存储用量，请刷新重试。";
    } finally {
      busy = refresh.disabled = owner.disabled = false;
      submit.disabled = !owner.value;
    }
  }

  owner.addEventListener("change", () => {
    result.hidden = true;
    hint.hidden = false;
    submit.disabled = !owner.value;
    statusOutput.textContent = "";
    const url = new URL(location.href);
    url.searchParams.delete("owner");
    history.replaceState(null, "", url);
  });
  form.addEventListener("submit", event => {
    event.preventDefault();
    if (owner.value) void load(false);
  });
  function openEditor(quota) {
    const limit = quota ? currentQuota?.limitMiB : currentAlert?.thresholdMiB;
    if (busy || limit === undefined) return;
    editingQuota = quota;
    alertForm.reset();
    alertForm.elements.threshold.value = limit;
    adminElement("admin-storage-alert-target").textContent = owner.value;
    adminElement("admin-storage-alert-dialog-title").textContent = quota ? "设置邮箱硬配额" : "设置容量预警";
    adminElement("admin-storage-alert-input-label").textContent = quota ? "硬配额（MiB）" : "容量阈值（MiB）";
    adminElement("admin-storage-alert-help").textContent = quota
      ? "超限会阻止增加用量的写入。现有邮件可继续读取，填 0 取消限制。"
      : "达到 80% 和 100% 时在此页提示。填 0 关闭预警，收发照常。";
    alertSave.textContent = quota ? "保存配额" : "保存阈值";
    alertError.textContent = "";
    alertEditor.showModal();
  }
  alertEdit.addEventListener("click", () => openEditor(false));
  adminElement("admin-storage-quota-edit").addEventListener("click", () => openEditor(true));
  alertClose.addEventListener("click", () => alertEditor.close());
  alertEditor.addEventListener("cancel", event => { if (alertSave.disabled) event.preventDefault(); });
  alertEditor.addEventListener("close", () => alertForm.reset());
  window.addEventListener("pageshow", event => { if (event.persisted && alertEditor.open) alertEditor.close(); });
  alertForm.addEventListener("submit", async event => {
    event.preventDefault();
    if (alertSave.disabled) return;
    const payload = {
      thresholdMiB: alertForm.elements.threshold.valueAsNumber,
      expectedMiB: editingQuota ? currentQuota.limitMiB : currentAlert.thresholdMiB,
      password: alertForm.elements.password.value
    };
    alertForm.elements.password.value = "";
    alertSave.disabled = alertClose.disabled = true;
    alertError.textContent = "";
    try {
      const path = "/api/admin/accounts/" + encodeURIComponent(owner.value) + (editingQuota ? "/storage-quota" : "/storage-alert");
      const response = await fetch(path, {
        method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload)
      });
      if (response.status === 401) {
        location.replace("/");
        return;
      }
      if (!response.ok) {
        const error = await response.json().catch(() => null);
        alertError.textContent = response.status === 409 ? "阈值已被修改。关闭窗口并刷新用量后重试。" :
          error?.error || "无法保存容量阈值，请重试。";
        return;
      }
      alertEditor.close();
      await load(false);
      if (!statusOutput.textContent) statusOutput.textContent = "已保存。";
    } catch {
      alertError.textContent = "未收到保存结果，请关闭窗口并刷新用量核对。";
    } finally {
      alertSave.disabled = alertClose.disabled = false;
    }
  });
  return () => load(true);
}
