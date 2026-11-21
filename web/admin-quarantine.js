"use strict";

function setupAdminQuarantine() {
  const items = adminElement("admin-quarantine-items");
  const more = adminElement("admin-quarantine-more");
  const dialog = adminElement("admin-quarantine-dialog");
  const form = adminElement("admin-quarantine-form");
  const submit = form.querySelector('[type="submit"]');
  let before = 0;
  let pending;

  async function load(cursor = 0) {
    more.disabled = refresh.disabled = true;
    try {
      const response = await fetch("/api/admin/quarantine?before=" + cursor);
      if (response.status === 401 || response.status === 403) return location.replace("/");
      if (!response.ok) throw new Error();
      const data = await response.json();
      adminElement("admin-identity").textContent = data.email;
      items.replaceChildren(...data.items.map(item => {
        const actions = document.createElement("div");
        actions.className = "admin-row-actions";
        for (const [action, label, title] of [
          ["discard", "删除", "永久删除隔离邮件"],
          ["release", "释放", "确认误报并释放到原收件人的垃圾邮件"]
        ]) {
          const button = document.createElement("button");
          button.type = "button";
          button.className = "secondary";
          button.textContent = label;
          button.addEventListener("click", () => {
            pending = { id: item.id, action };
            form.reset();
            adminElement("admin-quarantine-action").textContent = title;
            dialog.showModal();
          });
          actions.append(button);
        }
        return deliveryRow([new Date(item.stamp * 1000).toLocaleString(), item.sender || "空信封发件人",
          item.recipients.map(entry => entry.Address).join("、"), Math.ceil(item.size / 1024) + " KiB", actions]);
      }));
      before = data.items.at(-1)?.id || 0;
      more.hidden = data.items.length < 100;
      activePanel.hidden = false;
      statusOutput.textContent = data.items.length ? "" : "没有隔离邮件";
    } catch {
      statusOutput.textContent = "无法读取隔离邮件";
    } finally {
      more.disabled = refresh.disabled = false;
    }
  }

  more.addEventListener("click", () => load(before));
  adminElement("admin-quarantine-cancel").addEventListener("click", () => { if (!submit.disabled) dialog.close(); });
  dialog.addEventListener("cancel", event => { if (submit.disabled) event.preventDefault(); });
  form.addEventListener("submit", async event => {
    event.preventDefault();
    if (!pending || submit.disabled) return;
    const password = form.elements.password.value;
    form.elements.password.value = "";
    submit.disabled = true;
    try {
      const response = await fetch("/api/admin/quarantine/" + pending.id, {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ action: pending.action, password })
      });
      if (!response.ok) throw new Error((await response.json()).error);
      dialog.close();
      await load();
      void adminFeedback("已处理隔离邮件。");
    } catch (error) {
      void adminFeedback(error.message || "无法处置隔离邮件。", "error");
    } finally {
      submit.disabled = false;
    }
  });
  return () => load();
}
