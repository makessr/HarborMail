"use strict";

function setupAdminAliases() {
  const items = adminElement("admin-alias-items");
  const search = adminElement("admin-alias-search");
  const add = adminElement("admin-alias-add");
  const dialog = adminElement("admin-alias-dialog");
  const form = adminElement("admin-alias-form");
  const close = adminElement("admin-alias-close");
  const save = adminElement("admin-alias-save");
  const errorOutput = adminElement("admin-alias-error");
  const message = adminElement("admin-alias-status");
  const states = { active: "生效", shadowed: "被服务器配置覆盖", orphaned: "主邮箱已移除" };
  let aliases = [];
  let owners = [];
  let mode = "alias_add";

  function open(entry) {
    mode = entry ? "alias_remove" : "alias_add";
    form.reset();
    errorOutput.textContent = "";
    form.elements.owner.replaceChildren(...(entry ? [entry.owner] : owners).map(owner => new Option(owner, owner)));
    form.elements.owner.disabled = Boolean(entry);
    form.elements.alias.value = entry?.alias || "";
    form.elements.alias.readOnly = Boolean(entry);
    adminElement("admin-alias-title").textContent = entry ? "移除收信别名" : "添加收信别名";
    adminElement("admin-alias-impact").hidden = !entry;
    save.textContent = entry ? "确认移除" : "保存别名";
    dialog.showModal();
  }

  function render() {
    const query = search.value.trim().toLowerCase();
    const matching = aliases.filter(entry => entry.alias.includes(query) || entry.owner.includes(query));
    items.replaceChildren(...matching.map(entry => {
      let action = "—";
      if (entry.managed) {
        action = document.createElement("button");
        action.type = "button";
        action.className = "secondary";
        action.textContent = "移除";
        action.setAttribute("aria-label", "移除 " + entry.alias);
        action.addEventListener("click", () => open(entry));
      }
      return deliveryRow([entry.alias, entry.owner, entry.managed ? "后台" : "服务器配置", states[entry.state], action]);
    }));
    adminElement("admin-alias-empty").hidden = matching.length !== 0;
  }

  async function load() {
    refresh.disabled = add.disabled = true;
    statusOutput.textContent = "正在读取别名…";
    try {
      const response = await fetch("/api/admin/aliases");
      if (response.status === 401 || response.status === 403) {
        location.replace("/");
        return;
      }
      if (!response.ok) throw new Error("无法读取别名。");
      const data = await response.json();
      aliases = data.aliases;
      owners = data.owners;
      adminElement("admin-identity").textContent = data.email;
      render();
      activePanel.hidden = false;
      statusOutput.textContent = "";
      add.disabled = owners.length === 0;
    } catch {
      statusOutput.textContent = "无法读取别名，请刷新重试。";
    } finally {
      refresh.disabled = false;
    }
  }

  search.addEventListener("input", render);
  add.addEventListener("click", () => open());
  close.addEventListener("click", () => dialog.close());
  dialog.addEventListener("close", () => form.reset());
  dialog.addEventListener("cancel", event => { if (save.disabled) event.preventDefault(); });
  window.addEventListener("pageshow", event => { if (event.persisted && dialog.open) dialog.close(); });
  form.addEventListener("submit", async event => {
    event.preventDefault();
    if (save.disabled) return;
    const payload = {
      action: mode, alias: form.elements.alias.value.trim(),
      owner: form.elements.owner.value, password: form.elements.password.value
    };
    form.elements.password.value = "";
    close.disabled = save.disabled = true;
    errorOutput.textContent = "";
    try {
      const response = await fetch("/api/admin/aliases", {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload)
      });
      if (response.status === 401) {
        location.replace("/");
        return;
      }
      if (!response.ok) {
        const failure = await response.json().catch(() => null);
        errorOutput.textContent = failure?.error || "无法保存别名，请重试。";
        return;
      }
      dialog.close();
      message.textContent = mode === "alias_add" ? "已添加收信别名。" : "已移除收信别名。";
      await load();
    } catch {
      errorOutput.textContent = "未收到操作结果，请关闭窗口并刷新核对。";
    } finally {
      close.disabled = save.disabled = false;
    }
  });
  return load;
}
