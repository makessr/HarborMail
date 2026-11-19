"use strict";

{
  const dialog = byId("web-sessions-dialog");
  const status = byId("web-sessions-status");
  const list = byId("web-sessions-list");
  const refresh = byId("refresh-web-sessions");
  const revokeOthers = byId("revoke-other-sessions");
  let sessionItems = [];
  let generation = 0;
  let busy = false;
  let loading = false;

  function dateLabel(stamp) {
    return stamp ? fullDate.format(stamp * 1000) : "未记录";
  }

  function controls() {
    refresh.disabled = loading || busy;
    revokeOthers.disabled = loading || busy || !sessionItems.some(item => !item.current);
    for (const button of list.querySelectorAll("button")) button.disabled = loading || busy;
  }

  function sessionRow(item) {
    const row = document.createElement("li");
    const details = document.createElement("div");
    const title = document.createElement("strong");
    title.textContent = item.device || "升级前的会话";
    details.append(title);
    for (const text of [
      "登录时间 " + dateLabel(item.created),
      "最近活动 " + dateLabel(item.lastActive),
      "到期时间 " + dateLabel(item.expires),
    ]) {
      const label = document.createElement("p");
      label.className = "rule-help";
      label.textContent = text;
      details.append(label);
    }
    row.append(details);
    if (item.current) {
      const current = document.createElement("span");
      current.className = "web-sessions-current";
      current.textContent = "当前设备";
      row.append(current);
    } else {
      const button = document.createElement("button");
      button.type = "button";
      button.className = "secondary";
      button.textContent = "退出";
      button.setAttribute("aria-label", "退出 " + title.textContent + " 会话");
      button.addEventListener("click", () => void revoke(item.id, "退出此浏览器的登录会话？"));
      row.append(button);
    }
    return row;
  }

  async function loadSessions() {
    const request = ++generation;
    loading = true;
    status.textContent = "正在读取登录设备…";
    controls();
    try {
      const items = await api("web-sessions");
      if (request !== generation || !dialog.open) return;
      sessionItems = items;
      list.replaceChildren(...items.map(sessionRow));
      status.textContent = "";
    } catch (err) {
      if (request === generation && dialog.open) status.textContent = err.message;
    } finally {
      if (request === generation) {
        loading = false;
        controls();
      }
    }
  }

  async function revoke(id, message) {
    if (busy || loading || !confirm(message)) return;
    busy = true;
    controls();
    try {
      await api(id ? "web-sessions/" + id : "web-sessions", "DELETE");
      if (dialog.open) await loadSessions();
    } catch (err) {
      if (dialog.open) status.textContent = err.message;
    } finally {
      busy = false;
      controls();
    }
  }

  byId("show-web-sessions").addEventListener("click", () => {
    dialog.showModal();
    void loadSessions();
  });
  byId("close-web-sessions").addEventListener("click", () => dialog.close());
  dialog.addEventListener("close", () => {
    if (!dialog.open) {
      ++generation;
      loading = false;
      sessionItems = [];
      list.replaceChildren();
    }
  });
  refresh.addEventListener("click", () => void loadSessions());
  revokeOthers.addEventListener("click", () => void revoke("", "退出所有其他网页登录会话？当前设备会继续登录。"));
}
