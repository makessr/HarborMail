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
    const field = document.createElement("code");
    field.textContent = value;
    field.tabIndex = -1;
    function select() {
      field.focus();
      const range = document.createRange();
      range.selectNodeContents(field);
      const selection = window.getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
    }
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
          select();
          if (!document.execCommand("copy")) throw new Error();
        }
        status.textContent = "已复制" + label;
      } catch {
        select();
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
      const empty = record.purpose === "SPF" ? "在上方填 include 域名。仅收信的域可勾选「此域只收信」。" :
        record.purpose === "DKIM" ? (domainAdminOnly ? "请全站管理员打开此清单生成" : data.dkimAvailable ? "正在等待生成内置 DKIM" : "服务器需要配置 DKIM 加密密钥") :
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
    adminElement("admin-dns-plan-dkim-note").hidden = !data.records.some(record => record.purpose === "DKIM 待启用");
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
      const endpoint = "/api/admin/domains/" + encodeURIComponent(domain);
      async function readPlan() {
        const response = await fetch(endpoint + "/dns-plan?" + query, { signal: request.signal });
        if (response.status === 401) { location.replace("/"); throw new Error("请重新登录"); }
        const data = await response.json();
        if (!response.ok) throw new Error(data.error || "无法生成 DNS 记录");
        return data;
      }
      let data = await readPlan();
      if (!domainAdminOnly && data.dkimAvailable && data.dkimMissing) {
        const prepared = await fetch(endpoint + "/dkim/prepare", { method: "POST", signal: request.signal });
        if (!prepared.ok) {
          render(data);
          throw new Error((await prepared.json()).error || "无法生成 DKIM，请重试");
        }
        data = await readPlan();
      }
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
  dialog.addEventListener("close", () => { if (!dialog.open) pending?.abort(); });
  form.elements.receiveOnly.addEventListener("change", () => {
    form.elements.spfInclude.disabled = form.elements.receiveOnly.checked;
  });
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
    form.elements.spfInclude.disabled = false;
    adminElement("admin-dns-plan-title").textContent = "DNS 记录 · " + name;
    dialog.showModal();
    void load();
  };
}
