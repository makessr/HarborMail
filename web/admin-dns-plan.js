"use strict";

function setupDNSPlan() {
  const dialog = adminElement("admin-dns-plan-dialog");
  const form = adminElement("admin-dns-plan-form");
  const output = adminElement("admin-dns-plan-output");
  const records = adminElement("admin-dns-plan-records");
  const status = adminElement("admin-dns-plan-status");
  const generate = adminElement("admin-dns-plan-build");
  const dkim = adminElement("admin-dns-plan-dkim");
  let domain = "";
  let pending;

  function recordField(value, label, empty) {
    if (!value) return empty;
    const field = document.createElement("input");
    field.type = "text";
    field.readOnly = true;
    field.value = value;
    field.setAttribute("aria-label", label);
    const copy = document.createElement("button");
    copy.type = "button";
    copy.className = "secondary";
    copy.textContent = "复制";
    copy.setAttribute("aria-label", "复制" + label);
    copy.addEventListener("click", async () => {
      try {
        if (navigator.clipboard) {
          await navigator.clipboard.writeText(value);
        } else {
          field.focus();
          field.select();
          if (!document.execCommand("copy")) throw new Error();
        }
        status.textContent = "已复制" + label;
      } catch {
        field.focus();
        field.select();
        status.textContent = "已选中，请手动复制";
      }
    });
    const group = document.createElement("div");
    group.className = "admin-dns-record-field";
    group.append(field, copy);
    return group;
  }

  function render(data) {
    form.elements.host.value = data.host;
    records.replaceChildren(...data.records.map(record => {
      const empty = record.purpose === "SPF" ? "按中继商提供的值填写" :
        record.purpose === "DKIM" ? "按中继商提供的值填写，或生成内置 DKIM" :
        record.type === "MX" ? "请填写邮件主机名" : "请先填写公网 IP";
      const type = document.createElement("span");
      type.textContent = record.type;
      const purpose = document.createElement("small");
      purpose.textContent = record.priority ? "优先级 " + record.priority : record.purpose;
      type.append(purpose);
      return deliveryRow([type,
        recordField(record.name,"名称（" + record.purpose + "）",record.purpose === "DKIM" ? "按选择器确定" : "请填写邮件主机名"),
        recordField(record.value,"值（" + record.purpose + "）",empty)]);
    }));
    dkim.hidden = domainAdminOnly || !data.dkimAvailable;
    const keys = data.records.some(record => record.value && record.purpose.startsWith("DKIM"));
    adminElement("admin-dns-plan-dkim-note").hidden = !keys;
    output.hidden = false;
  }

  async function load() {
    pending?.abort();
    const request = new AbortController();
    pending = request;
    status.textContent = "正在生成 DNS 记录…";
    output.hidden = true;
    generate.disabled = true;
    try {
      const query = new URLSearchParams(new FormData(form));
      const response = await fetch("/api/admin/domains/" + encodeURIComponent(domain) + "/dns-plan?" + query,
        { signal: request.signal });
      if (response.status === 401) return location.replace("/");
      const data = await response.json();
      if (!response.ok) throw new Error(data.error || "无法生成 DNS 记录");
      if (pending !== request) return;
      render(data);
      status.textContent = "";
    } catch (error) {
      if (!request.signal.aborted) status.textContent = error.message || "无法生成 DNS 记录";
    } finally {
      if (pending === request) generate.disabled = false;
    }
  }

  adminElement("admin-dns-plan-close").addEventListener("click", () => dialog.close());
  dialog.addEventListener("close", () => pending?.abort());
  form.addEventListener("submit", event => { event.preventDefault(); void load(); });
  dkim.addEventListener("click", () => {
    dialog.close();
    void openDKIMEditor(domain);
  });
  adminElement("admin-dns-plan-check").addEventListener("click", () => {
    dialog.close();
    if (!dnsDomain.disabled && Array.from(dnsDomain.options).some(option => option.value === domain)) {
      dnsDomain.value = domain;
      dnsDomain.dispatchEvent(new Event("change"));
    }
    dnsDomain.focus();
    adminElement("admin-dns-title").scrollIntoView({ block: "nearest" });
  });
  return name => {
    domain = name;
    form.reset();
    adminElement("admin-dns-plan-title").textContent = "DNS 记录 · " + name;
    dialog.showModal();
    void load();
  };
}
