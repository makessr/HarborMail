"use strict";

function setupAdminAliases() {
  const items = adminElement("admin-alias-items");
  const empty = adminElement("admin-alias-empty");
  const search = adminElement("admin-alias-search");
  const add = adminElement("admin-alias-add");
  const dialog = adminElement("admin-alias-dialog");
  const form = adminElement("admin-alias-form");
  const close = adminElement("admin-alias-close");
  const save = adminElement("admin-alias-save");
  const states = { active: "生效", shadowed: "被服务器配置覆盖", orphaned: "主邮箱已移除" };
  let aliases = [];
  let owners = [];
  let mode = "alias_add";
  search.value = new URLSearchParams(location.search).get("search") || "";

  const titles = { alias_add: "添加收信别名", alias_remove: "移除收信别名",
    alias_send_grant: "授权别名发信", alias_send_revoke: "撤销别名发信" };

  function open(entry, operation = entry ? "alias_remove" : "alias_add") {
    mode = operation;
    form.reset();
    form.elements.owner.replaceChildren(...(entry ? [entry.owner] : owners).map(owner => new Option(owner, owner)));
    form.elements.owner.disabled = Boolean(entry);
    form.elements.alias.value = entry?.alias || "";
    form.elements.alias.readOnly = Boolean(entry);
    adminElement("admin-alias-title").textContent = titles[mode];
    adminElement("admin-alias-impact").hidden = mode !== "alias_remove";
    save.textContent = "确认";
    dialog.showModal();
  }

  function render() {
    const query = search.value.trim().toLowerCase();
    const matching = aliases.filter(entry => entry.alias.includes(query) || entry.owner.includes(query));
    items.replaceChildren(...matching.map(entry => {
      const actions = document.createElement("div");
      actions.className = "admin-row-actions";
      if (entry.state === "active") {
        const toggle = document.createElement("button");
        toggle.type = "button";
        toggle.className = "secondary";
        toggle.textContent = entry.sendAs ? "撤销发信" : "授权发信";
        toggle.addEventListener("click", () => open(entry, entry.sendAs ? "alias_send_revoke" : "alias_send_grant"));
        actions.append(toggle);
      }
      if (entry.managed) {
        const action = document.createElement("button");
        action.type = "button";
        action.className = "secondary";
        action.textContent = "移除";
        action.setAttribute("aria-label", "移除 " + entry.alias);
        action.addEventListener("click", () => open(entry));
        actions.append(action);
      }
      return deliveryRow([entry.alias, entry.owner, entry.managed ? "后台" : "服务器配置", states[entry.state], entry.sendAs ? "已授权" : "未授权", actions.childElementCount ? actions : "—"]);
    }));
    empty.textContent = aliases.length ? "没有匹配的别名" :
      (owners.length ? "暂无收信别名" : "请先创建主邮箱");
    empty.hidden = matching.length !== 0;
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
        void adminFeedback(failure?.error || "无法保存别名，请重试。", "error");
        return;
      }
      dialog.close();
      await load();
      void adminFeedback(titles[mode] + "已完成。");
    } catch {
      void adminFeedback("未收到操作结果，请刷新核对。", "error");
    } finally {
      close.disabled = save.disabled = false;
    }
  });
  return load;
}
