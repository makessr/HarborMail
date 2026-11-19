"use strict";

let mailboxLifecycleAccount;

function renderMailboxLifecycle() {
  const form = adminElement("admin-mailbox-lifecycle-form");
  const action = form.elements.action.value;
  const rename = action === "mailbox_rename";
  const purge = action === "mailbox_purge";
  adminElement("admin-mailbox-lifecycle-destination").hidden = !rename;
  adminElement("admin-mailbox-lifecycle-confirm").hidden = !purge;
  form.elements.destination.required = form.elements.destination.disabled = false;
  form.elements.confirm.required = form.elements.confirm.disabled = false;
  if (rename) form.elements.destination.required = true;
  else form.elements.destination.disabled = true;
  if (purge) form.elements.confirm.required = true;
  else form.elements.confirm.disabled = true;
  adminElement("admin-mailbox-lifecycle-impact").textContent = {
    mailbox_rename: "邮件和设置迁移到新地址。旧地址保留为收信别名，原会话及应用密码失效。",
    mailbox_retire: "暂停登录和收信，保留邮件、设置和别名。仍可通过后台恢复。",
    mailbox_restore: "恢复邮箱与别名的收信能力，并恢复回收前的登录状态。",
    mailbox_purge: "永久清除这个已回收邮箱的数据和别名。旧地址继续保留，无法分给新邮箱。"
  }[action];
}

function openMailboxLifecycle(account) {
  const form = adminElement("admin-mailbox-lifecycle-form");
  mailboxLifecycleAccount = account;
  form.reset();
  adminElement("admin-mailbox-lifecycle-target").textContent = account.email;
  adminElement("admin-mailbox-lifecycle-error").textContent = "";
  const actions = account.retired ? ["mailbox_restore", "mailbox_purge"] : ["mailbox_rename", "mailbox_retire"];
  form.elements.action.replaceChildren(...actions.map(action => new Option(accessLabels[action], action)));
  renderMailboxLifecycle();
  adminElement("admin-mailbox-lifecycle").showModal();
}

function setupMailboxLifecycle() {
  const dialog = adminElement("admin-mailbox-lifecycle");
  const form = adminElement("admin-mailbox-lifecycle-form");
  const close = adminElement("admin-mailbox-lifecycle-close");
  const save = form.querySelector('[type="submit"]');
  const output = adminElement("admin-mailbox-lifecycle-error");
  close.addEventListener("click", () => dialog.close());
  form.elements.action.addEventListener("change", renderMailboxLifecycle);
  dialog.addEventListener("close", () => form.reset());
  dialog.addEventListener("cancel", event => { if (save.disabled) event.preventDefault(); });
  window.addEventListener("pageshow", event => { if (event.persisted && dialog.open) dialog.close(); });
  form.addEventListener("submit", async event => {
    event.preventDefault();
    if (save.disabled) return;
    const data = Object.fromEntries(new FormData(form));
    const target = mailboxLifecycleAccount.email;
    form.elements.password.value = "";
    close.disabled = save.disabled = true;
    output.textContent = "";
    try {
      const response = await fetch("/api/admin/accounts/" + encodeURIComponent(target) + "/lifecycle", {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data)
      });
      if (response.status === 401) {
        location.replace("/");
        return;
      }
      if (!response.ok) {
        const error = await response.json().catch(() => null);
        output.textContent = error?.error || "无法保存邮箱状态，请刷新重试。";
        return;
      }
      dialog.close();
      if (data.action === "mailbox_rename") {
        accountDomain.value = "";
        find.value = data.destination.trim().toLowerCase();
      }
      await loadOverview();
      adminElement("admin-account-created").textContent = accessLabels[data.action] + "已完成。";
    } catch {
      output.textContent = "未收到操作结果，请关闭窗口并刷新账号列表核对。";
    } finally {
      close.disabled = save.disabled = false;
    }
  });
}
