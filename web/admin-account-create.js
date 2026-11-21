"use strict";

function setupAdminAccountCreate() {
  const dialog = adminElement("admin-account-add-dialog");
  const form = adminElement("admin-account-add-form");
  const close = adminElement("admin-account-add-close");
  const save = adminElement("admin-account-add-save");
  const errorOutput = adminElement("admin-account-add-error");
  const addressLabel = adminElement("admin-account-add-address-label");
  const suffix = adminElement("admin-account-add-domain");
  const fields = form.elements;
  let selectedDomain = "";

  function open(domain = "") {
    form.reset();
    selectedDomain = domain;
    fields.email.type = domain ? "text" : "email";
    fields.email.maxLength = domain ? 253 - domain.length : 254;
    addressLabel.textContent = domain ? "邮箱用户名" : "主邮箱";
    suffix.textContent = domain ? "邮箱后缀 @" + domain : "";
    suffix.hidden = !domain;
    errorOutput.textContent = "";
    dialog.showModal();
    fields.email.focus();
  }
  adminElement("admin-account-add").addEventListener("click", () => open());
  close.addEventListener("click", () => dialog.close());
  dialog.addEventListener("cancel", event => { if (save.disabled) event.preventDefault(); });
  dialog.addEventListener("close", () => form.reset());
  window.addEventListener("pageshow", event => { if (event.persisted && dialog.open) dialog.close(); });

  form.addEventListener("submit", async event => {
    event.preventDefault();
    if (save.disabled) return;
    if (fields.initialPassword.value !== fields.confirmPassword.value) {
      errorOutput.textContent = "两次输入的初始密码不一致。";
      fields.confirmPassword.focus();
      return;
    }
    const length = new TextEncoder().encode(fields.initialPassword.value).length;
    if (length < 12 || length > 72) {
      errorOutput.textContent = "初始密码长度须为 12 至 72 字节。";
      fields.initialPassword.focus();
      return;
    }
    const data = {
      email: fields.email.value.trim() + (selectedDomain ? "@" + selectedDomain : ""),
      initialPassword: fields.initialPassword.value, password: fields.password.value
    };
    fields.initialPassword.value = fields.confirmPassword.value = fields.password.value = "";
    close.disabled = save.disabled = true;
    errorOutput.textContent = "";
    try {
      const response = await fetch("/api/admin/accounts", {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data)
      });
      if (response.status === 401) {
        location.replace("/");
        return;
      }
      const result = await response.json();
      if (!response.ok) {
        void adminFeedback(result.error || "无法创建账号，请重试。", "error");
        return;
      }
      dialog.close();
      accountDomain.value = "";
      accountRole.value = "";
      accountStatus.value = "";
      find.value = result.email;
      await loadOverview();
      void adminFeedback("已创建 " + result.email + "。");
    } catch {
      void adminFeedback("未收到创建结果，请刷新账号列表核对。", "error");
    } finally {
      close.disabled = save.disabled = false;
    }
  });
  return open;
}
