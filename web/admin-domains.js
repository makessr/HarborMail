"use strict";

function renderDomainRows(domains) {
  const content = document.createDocumentFragment();
  for (const { name, accounts, aliases, managed } of domains) {
    const row = document.createElement("tr");
    row.insertCell().textContent = name;
    row.insertCell().textContent = accounts;
    row.insertCell().textContent = aliases;
    row.insertCell().textContent = managed ? "后台登记" : "邮箱或别名";

    const cell = row.insertCell();
    const actions = document.createElement("div");
    actions.className = "admin-domain-actions";
    actions.append(
      overviewLink("查看账号", "/admin/accounts?" + new URLSearchParams({ domain: name })),
      overviewLink("新建邮箱", "/admin/accounts?" + new URLSearchParams({ domain: name, create: "1" }))
    );
    const dns = document.createElement("button");
    dns.type = "button";
    dns.className = "secondary";
    dns.textContent = "DNS 记录";
    dns.setAttribute("aria-label", "配置 " + name + " 的 DNS 记录");
    dns.addEventListener("click", () => openDNSPlan(name));
    actions.append(dns);
    if (!domainAdminOnly) {
      const dkim = document.createElement("button");
      dkim.type = "button";
      dkim.className = "secondary";
      dkim.textContent = "DKIM";
      dkim.setAttribute("aria-label", "管理 " + name + " 的 DKIM");
      dkim.addEventListener("click", () => openDKIMEditor(name));
      actions.append(dkim);
      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "secondary admin-domain-remove";
      remove.textContent = "删除域名";
      remove.setAttribute("aria-label", "删除域名 " + name);
      remove.addEventListener("click", () => openDomainEditor({ name, accounts, aliases, managed }));
      actions.append(remove);
    }
    cell.append(actions);
    content.append(row);
  }
  adminElement("admin-domain-list").replaceChildren(content);
}

function setupAdminDKIM() {
  const dialog = adminElement("admin-dkim-dialog");
  const form = adminElement("admin-dkim-form");
  const close = adminElement("admin-dkim-close");
  const status = adminElement("admin-dkim-status");
  const copyStatus = adminElement("admin-dkim-copy-status");
  const controls = adminElement("admin-dkim-controls");
  const dns = adminElement("admin-dkim-dns");
  const { password } = form.elements;
  const buttons = [...form.querySelectorAll("button[name=action]")];
  const [generate, enable, disable] = buttons;
  let domain = "";
  let current = {};

  function render(data) {
    current = data;
    const record = data.pending || data.active;
    renderDetails("admin-dkim-state", [
      ["当前选择器", data.active?.selector || "未启用"],
      ...(data.pending ? [["待启用选择器", data.pending.selector]] : [])
    ]);
    dns.hidden = !record;
    dns.replaceChildren();
    copyStatus.textContent = "";
    if (record) {
      for (const [label, value] of [["TXT 记录名", record.name], ["TXT 记录值", record.txt]]) {
        const row = document.createElement("div");
        row.className = "admin-dkim-record";
        const title = document.createElement("span");
        title.textContent = label;
        row.append(title, adminDNSRecordField(value, label, copyStatus));
        dns.append(row);
      }
    }
    controls.hidden = !data.available && !data.active;
    generate.hidden = !data.available;
    generate.textContent = data.pending ? "重新生成待启用密钥" : data.active ? "生成轮换密钥" : "生成密钥";
    enable.hidden = !data.available || !data.pending;
    disable.hidden = !data.active;
    status.textContent = data.available
      ? (data.pending ? "添加上面的 TXT 记录，再检查 DNS 并启用。" : "")
      : "请先在服务器配置 DKIM 加密密钥。";
  }

  function busy(value) {
    close.disabled = password.disabled = value;
    for (const button of buttons) button.disabled = value;
  }

  async function request(data) {
    const endpoint = "/api/admin/domains/" + encodeURIComponent(domain) + "/dkim";
    const response = await fetch(endpoint, data ? {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data)
    } : undefined);
    if (response.status === 401) {
      location.replace("/");
      return;
    }
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "无法读取 DKIM 配置");
    render(result);
  }

  close.addEventListener("click", () => dialog.close());
  dialog.addEventListener("cancel", event => { if (close.disabled) event.preventDefault(); });
  dialog.addEventListener("close", () => form.reset());
  form.addEventListener("submit", async event => {
    event.preventDefault();
    if (close.disabled || !event.submitter?.value) return;
    const action = event.submitter.value;
    const selector = action === "disable" ? current.active?.selector : current.pending?.selector;
    const data = { action, selector, password: password.value };
    password.value = "";
    busy(true);
    status.textContent = "";
    try {
      await request(data);
      if (action === "enable") void adminFeedback("DKIM 签名已启用。");
      if (action === "disable") void adminFeedback("DKIM 签名已停用。");
      if (action === "generate") void adminFeedback("已生成待启用的 DKIM 密钥。");
    } catch (error) {
      void adminFeedback(error.message, "error");
    } finally {
      busy(false);
    }
  });

  return async name => {
    domain = name;
    render({});
    adminElement("admin-dkim-title").textContent = "DKIM 签名 · " + domain;
    status.textContent = "正在读取 DKIM 配置…";
    dialog.showModal();
    busy(true);
    try {
      await request();
    } catch (error) {
      status.textContent = error.message;
    } finally {
      busy(false);
      close.focus();
    }
  };
}

function setupAdminDomains() {
  const dialog = adminElement("admin-domain-dialog");
  const form = adminElement("admin-domain-form");
  const close = adminElement("admin-domain-close");
  const cancel = adminElement("admin-domain-cancel");
  const save = adminElement("admin-domain-save");
  const mailboxLink = adminElement("admin-domain-mailboxes");
  const aliasLink = adminElement("admin-domain-aliases");
  const { domain, password } = form.elements;
  let removing = false;

  function open(entry) {
    form.reset();
    removing = Boolean(entry);
    const blocked = removing && (!entry.managed || entry.accounts > 0 || entry.aliases > 0);
    domain.value = entry?.name || "";
    domain.readOnly = removing;
    const title = removing ? "删除域名" : "新增域名";
    adminElement("admin-domain-dialog-title").textContent = title;
    const remaining = [entry?.accounts && entry.accounts + " 个邮箱", entry?.aliases && entry.aliases + " 个别名"].filter(Boolean).join("和");
    adminElement("admin-domain-impact").textContent = blocked
      ? "此域名还有" + remaining + "。请先迁移或清除这些地址。" + (entry.managed ? "" : "处理完后，自动生成的域名会从列表消失。")
      : removing ? "将从 Harbor Mail 删除此域名及内置 DKIM 密钥。公网 DNS 由域名服务商管理。"
      : "登记后会显示收发邮件所需的 DNS 记录。";
    save.toggleAttribute("data-destructive", removing);
    cancel.hidden = !removing;
    cancel.textContent = blocked ? "关闭" : "取消";
    save.textContent = title;
    save.hidden = password.disabled = blocked;
    adminElement("admin-domain-password").hidden = blocked;
    mailboxLink.hidden = !blocked || !entry.accounts;
    aliasLink.hidden = !blocked || !entry.aliases;
    mailboxLink.href = "/admin/accounts?" + new URLSearchParams({ domain: domain.value });
    aliasLink.href = "/admin/aliases?" + new URLSearchParams({ search: "@" + domain.value });
    dialog.showModal();
    if (blocked) (mailboxLink.hidden ? aliasLink : mailboxLink).focus();
    else (removing ? password : domain).focus();
  }

  adminElement("admin-domain-add").addEventListener("click", () => open());
  close.addEventListener("click", () => dialog.close());
  cancel.addEventListener("click", () => dialog.close());
  dialog.addEventListener("cancel", event => { if (save.disabled) event.preventDefault(); });
  dialog.addEventListener("close", () => form.reset());
  window.addEventListener("pageshow", event => { if (event.persisted && dialog.open) dialog.close(); });

  form.addEventListener("submit", async event => {
    event.preventDefault();
    if (save.disabled || save.hidden) return;
    const remove = removing;
    const data = { domain: domain.value.trim(), password: password.value, action: remove ? "domain_remove" : "domain_add" };
    password.value = "";
    close.disabled = cancel.disabled = save.disabled = true;
    try {
      const response = await fetch("/api/admin/domains", {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data)
      });
      if (response.status === 401) {
        location.replace("/");
        return;
      }
      if (!response.ok) {
        const result = await response.json().catch(() => null);
        void adminFeedback(result?.error || "无法保存域名，请重试。", "error");
        return;
      }
      const name = remove ? data.domain : (await response.json()).name;
      dialog.close();
      await loadOverview();
      await adminFeedback((remove ? "已删除 " : "已登记 ") + name + "。");
      if (!remove) void openDNSPlan(name);
    } catch {
      void adminFeedback("未收到保存结果，请刷新域名列表核对。", "error");
    } finally {
      close.disabled = cancel.disabled = save.disabled = false;
    }
  });
  return open;
}
