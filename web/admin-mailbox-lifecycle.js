"use strict";

let mailboxLifecycleAccount;

function renderMailboxLifecycle() {
  const form = adminElement("admin-mailbox-lifecycle-form");
  const action = form.elements.action.value;
  const rename = action === "mailbox_rename";
  const purge = action === "mailbox_purge";
  const handoff = action === "mailbox_retire" && mailboxLifecycleAccount.configuredAdmin;
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
    mailbox_retire: handoff ? "请先授予另一邮箱全站管理员权限，并用该账号在另一浏览器登录后台。回收后当前账号将退出，邮件会保留，接手人可恢复或永久删除。" : "暂停登录和收信，保留邮件、设置和别名。仍可通过后台恢复。",
    mailbox_restore: mailboxLifecycleAccount.configuredAdmin ? "恢复邮箱、收信和安装时配置的管理员权限。" : "恢复邮箱与别名的收信能力，并恢复回收前的登录状态。",
    mailbox_purge: "永久清除这个已回收邮箱的数据和别名。旧地址继续保留，无法分给新邮箱。"
  }[action];
  const button = form.querySelector('[type="submit"]');
  button.textContent = handoff ? "交接并回收" : accessLabels[action];
  button.toggleAttribute("data-destructive", purge);
}

function openMailboxLifecycle(account) {
  const form = adminElement("admin-mailbox-lifecycle-form");
  mailboxLifecycleAccount = account;
  form.reset();
  adminElement("admin-mailbox-lifecycle-target").textContent = account.email;
  const actions = account.retired ? ["mailbox_restore", "mailbox_purge"] : account.managed ? ["mailbox_rename", "mailbox_retire"] : ["mailbox_retire"];
  form.elements.action.replaceChildren(...actions.map(action => new Option(accessLabels[action], action)));
  renderMailboxLifecycle();
  adminElement("admin-mailbox-lifecycle").showModal();
}

function setupMailboxLifecycle() {
  const dialog = adminElement("admin-mailbox-lifecycle");
  const form = adminElement("admin-mailbox-lifecycle-form");
  const close = adminElement("admin-mailbox-lifecycle-close");
  const save = form.querySelector('[type="submit"]');
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
        void adminFeedback(error?.error || "无法保存邮箱状态，请刷新重试。", "error");
        return;
      }
      dialog.close();
      if (data.action === "mailbox_retire" && target === signedInAdmin) {
        await adminFeedback("已回收当前邮箱。请用接手的管理员账号打开后台，完成后续操作。");
        location.replace("/");
        return;
      }
      if (data.action === "mailbox_rename") {
        accountDomain.value = "";
        find.value = data.destination.trim().toLowerCase();
      }
      await loadOverview();
      void adminFeedback(accessLabels[data.action] + "已完成。");
    } catch {
      void adminFeedback("未收到操作结果，请刷新账号列表核对。", "error");
    } finally {
      close.disabled = save.disabled = false;
    }
  });
}
