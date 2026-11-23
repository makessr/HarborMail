"use strict";

{
  const loginForm = byId("login-form");
  const loginStatus = byId("login-password-status");
  const output = byId("recovery-code-output");
  const secret = byId("recovery-code-secret");
  const copyStatus = byId("recovery-code-copy-status");

  function loginAfterPasswordChange(email, message) {
    showLogin();
    loginForm.reset();
    loginForm.elements.email.value = email;
    byId("login-mfa-field").hidden = true;
    byId("login-error").textContent = "";
    loginStatus.textContent = message;
    loginForm.elements.password.focus();
  }

  function setupDialog({ id, opener, action, prepare, reset }) {
    const dialog = byId(id + "-dialog");
    const form = byId(id + "-form");
    const errorID = id + "-error";
    const button = form.querySelector('button[type="submit"]');
    const close = dialog.querySelector("[data-close]");
    byId(opener).addEventListener("click", () => {
      prepare?.(form);
      dialog.showModal();
    });
    close.addEventListener("click", () => dialog.close());
    dialog.addEventListener("cancel", event => { if (button.disabled) event.preventDefault(); });
    dialog.addEventListener("close", () => {
      form.reset();
      byId(errorID).textContent = "";
      reset?.();
    });
    form.addEventListener("submit", event => {
      event.preventDefault();
      if (button.disabled) return;
      close.disabled = true;
      void submit(form, async data => {
        if (data.newPassword !== undefined) {
          if (data.newPassword !== data.confirmation) {
            form.elements.confirmation.focus();
            throw new Error("两次输入的新密码不一致。");
          }
          const bytes = new TextEncoder().encode(data.newPassword).length;
          if (bytes < 12 || bytes > 72) {
            form.elements.newPassword.focus();
            throw new Error("新密码长度须为 12 至 72 字节。");
          }
          delete data.confirmation;
        }
        if (form.elements.password) form.elements.password.value = "";
        await action(data, dialog, form);
      }, errorID).finally(() => { close.disabled = false; });
    });
  }

  setupDialog({
    id: "password-settings", opener: "show-password-settings",
    async action(data, dialog) {
      const email = byId("account").textContent;
      await api("password", "POST", data);
      dialog.close();
      loginAfterPasswordChange(email, "密码已修改，请用新密码登录。");
    }
  });
  setupDialog({
    id: "recovery-code", opener: "show-recovery-code",
    async action(data, dialog, form) {
      const result = await api("password/recovery-code", "POST", data);
      if (!dialog.open) return;
      form.hidden = true;
      secret.value = result.code;
      output.hidden = false;
      secret.focus();
      secret.select();
    },
    reset() {
      byId("recovery-code-form").hidden = false;
      output.hidden = true;
      secret.value = "";
      copyStatus.textContent = "";
    }
  });
  setupDialog({
    id: "password-recovery", opener: "show-password-recovery",
    prepare(form) { form.elements.email.value = loginForm.elements.email.value; },
    async action(data, dialog) {
      await api("password/recover", "POST", data);
      dialog.close();
      loginAfterPasswordChange(data.email, "密码已重设，请用新密码登录。");
    }
  });
  byId("copy-recovery-code").addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(secret.value);
      copyStatus.textContent = "已复制。";
    } catch {
      secret.focus();
      secret.select();
      copyStatus.textContent = "请手动复制已选中的重置码。";
    }
  });
}
