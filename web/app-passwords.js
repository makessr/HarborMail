"use strict";

{
  const dialog = byId("app-passwords-dialog");
  const overview = byId("app-passwords-overview");
  const status = byId("app-passwords-status");
  const list = byId("app-passwords-list");
  const form = byId("app-password-form");
  const reveal = byId("app-password-reveal");
  const secret = byId("app-password-secret");
  const close = byId("close-app-passwords");
  const cancel = byId("cancel-app-password");
  const save = form.querySelector("button[type=submit]");
  const copyStatus = byId("app-password-copy-status");
  const protocolLabels = { imap: "IMAP", pop3: "POP3", smtp: "SMTP 提交" };
  let request = 0;

  async function revoke(item, button) {
    if (!confirm("撤销应用密码「" + item.label + "」？")) return;
    const current = request;
    button.disabled = true;
    try {
      await api("app-passwords/" + item.id, "DELETE");
      if (dialog.open && !overview.hidden) await showPasswords();
    } catch (err) {
      if (current === request && dialog.open) status.textContent = err.message;
    } finally {
      button.disabled = false;
    }
  }

  function rowForPassword(item) {
    const row = document.createElement("li");
    const details = document.createElement("div");
    const label = document.createElement("strong");
    label.textContent = item.label;
    details.append(label);
    const expires = item.expires ? fullDate.format(item.expires * 1000) : "直到撤销";
    const expiration = item.expires && item.expires * 1000 <= Date.now() ? "已过期，" + expires : expires;
    for (const value of [
      item.protocols.map(name => protocolLabels[name]).join("、"),
      "有效期 " + expiration,
      "最近使用 " + (item.lastUsed ? fullDate.format(item.lastUsed * 1000) : "从未使用"),
    ]) {
      const text = document.createElement("p");
      text.className = "rule-help";
      text.textContent = value;
      details.append(text);
    }
    const button = document.createElement("button");
    button.type = "button";
    button.className = "secondary";
    button.textContent = "撤销";
    button.setAttribute("aria-label", "撤销应用密码 " + item.label);
    button.addEventListener("click", () => void revoke(item, button));
    row.append(details, button);
    return row;
  }

  async function showPasswords() {
    const current = ++request;
    secret.value = "";
    form.reset();
    form.hidden = true;
    reveal.hidden = true;
    overview.hidden = false;
    list.replaceChildren();
    status.textContent = "正在读取应用密码…";
    try {
      const items = await api("app-passwords");
      if (request !== current || !dialog.open) return;
      list.replaceChildren(...items.map(rowForPassword));
      status.textContent = items.length ? "" : "还没有应用密码。";
    } catch (err) {
      if (request === current && dialog.open) status.textContent = err.message;
    }
  }

  byId("show-app-passwords").addEventListener("click", () => {
    dialog.showModal();
    void showPasswords();
  });
  close.addEventListener("click", () => dialog.close());
  dialog.addEventListener("cancel", event => { if (close.disabled) event.preventDefault(); });
  dialog.addEventListener("close", () => {
    if (!dialog.open) {
      ++request;
      form.reset();
      secret.value = "";
      list.replaceChildren();
    }
  });
  byId("new-app-password").addEventListener("click", () => {
    ++request;
    overview.hidden = true;
    form.hidden = false;
    byId("app-password-error").textContent = "";
    form.elements.label.focus();
  });
  cancel.addEventListener("click", () => void showPasswords());
  byId("finish-app-password").addEventListener("click", () => void showPasswords());
  form.addEventListener("submit", async event => {
    event.preventDefault();
    if (save.disabled) return;
    const error = byId("app-password-error");
    const data = new FormData(form);
    const protocols = data.getAll("protocols");
    if (!protocols.length) {
      error.textContent = "至少选择一个授权协议";
      return;
    }
    const current = request;
    for (const button of [save, close, cancel]) button.disabled = true;
    error.textContent = "";
    form.elements.password.value = "";
    try {
      const result = await api("app-passwords", "POST", {
        label: data.get("label"), password: data.get("password"), days: Number(data.get("days")), protocols,
      });
      if (request !== current || !dialog.open) return;
      form.reset();
      form.hidden = true;
      copyStatus.textContent = "";
      secret.value = result.secret;
      reveal.hidden = false;
      byId("copy-app-password").focus();
    } catch (err) {
      if (request === current && dialog.open) {
        error.textContent = err.message;
        form.elements.password.focus();
      }
    } finally {
      for (const button of [save, close, cancel]) button.disabled = false;
    }
  });
  byId("copy-app-password").addEventListener("click", async () => {
    const value = secret.value;
    if (!value) return;
    try {
      await navigator.clipboard.writeText(value);
      if (secret.value === value) copyStatus.textContent = "已复制应用密码。";
    } catch {
      if (secret.value !== value) return;
      secret.focus();
      secret.select();
      copyStatus.textContent = "已选中应用密码，请手动复制。";
    }
  });
}
