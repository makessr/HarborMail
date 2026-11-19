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
    const dns = overviewLink("检查 DNS", "#admin-dns-title");
    dns.addEventListener("click", () => {
      if (dnsDomain.disabled) return;
      if (dnsDomain.value !== name) {
        dnsDomain.value = name;
        dnsResults.hidden = true;
        dnsStatus.textContent = "";
      }
      dnsDomain.focus();
    });
    actions.append(dns);
    if (!domainAdminOnly && managed && accounts === 0 && aliases === 0) {
      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "secondary";
      remove.textContent = "移除";
      remove.setAttribute("aria-label", "移除 " + name);
      remove.addEventListener("click", () => openDomainEditor(name));
      actions.append(remove);
    }
    cell.append(actions);
    content.append(row);
  }
  adminElement("admin-domain-list").replaceChildren(content);
}

function setupAdminDomains() {
  const dialog = adminElement("admin-domain-dialog");
  const form = adminElement("admin-domain-form");
  const close = adminElement("admin-domain-close");
  const save = adminElement("admin-domain-save");
  const errorOutput = adminElement("admin-domain-error");
  const status = adminElement("admin-domain-status");
  const { domain, password } = form.elements;
  let removing = false;

  function open(name = "") {
    form.reset();
    removing = Boolean(name);
    domain.value = name;
    domain.readOnly = removing;
    const title = removing ? "移除域名" : "新增域名";
    adminElement("admin-domain-dialog-title").textContent = title;
    adminElement("admin-domain-impact").textContent = removing
      ? "移除后，此域名将从后台列表中消失。"
      : "登记后可检查 DNS 和创建邮箱。公网收信需要配置 MX。";
    save.textContent = title;
    errorOutput.textContent = "";
    dialog.showModal();
    (removing ? password : domain).focus();
  }

  adminElement("admin-domain-add").addEventListener("click", () => open());
  close.addEventListener("click", () => dialog.close());
  dialog.addEventListener("cancel", event => { if (save.disabled) event.preventDefault(); });
  dialog.addEventListener("close", () => form.reset());
  window.addEventListener("pageshow", event => { if (event.persisted && dialog.open) dialog.close(); });

  form.addEventListener("submit", async event => {
    event.preventDefault();
    if (save.disabled) return;
    const remove = removing;
    const data = { domain: domain.value.trim(), password: password.value, action: remove ? "domain_remove" : "domain_add" };
    password.value = "";
    close.disabled = save.disabled = true;
    errorOutput.textContent = "";
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
        errorOutput.textContent = result?.error || "无法保存域名，请重试。";
        return;
      }
      const name = remove ? data.domain : (await response.json()).name;
      dialog.close();
      status.textContent = (remove ? "已移除 " : "已登记 ") + name + "。";
      await loadOverview();
      if (!remove && Array.from(dnsDomain.options).some(option => option.value === name)) {
        dnsDomain.value = name;
        dnsResults.hidden = true;
        dnsStatus.textContent = "";
      }
    } catch {
      errorOutput.textContent = "未收到保存结果，请关闭窗口并刷新域名列表核对。";
    } finally {
      close.disabled = save.disabled = false;
    }
  });
  return open;
}
