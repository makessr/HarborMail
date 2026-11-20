"use strict";

{
  const dialog = byId("client-settings-dialog");
  const status = byId("client-settings-status");
  const copy = byId("copy-client-settings");
  const list = byId("client-settings-values");
  let clipboard = "";
  byId("show-client-settings").addEventListener("click", async () => {
    dialog.showModal();
    list.replaceChildren();
    copy.disabled = true;
    status.textContent = "正在读取配置…";
    try {
      const { host, username, protocols } = await api("client-settings");
      if (!host || protocols.length === 0) {
        status.textContent = "请向管理员索取客户端地址和端口。";
        return;
      }
      const rows = [["服务器", host], ["登录用户名", username],
        ...protocols.map(item => [item.name + " · " + item.tls, String(item.port)])];
      for (const [label, value] of rows) {
        const term = document.createElement("dt");
        const detail = document.createElement("dd");
        term.textContent = label;
        detail.textContent = value;
        list.append(term, detail);
      }
      clipboard = rows.map(([label, value]) => label + "  " + value).join("\n");
      copy.disabled = false;
      status.textContent = "";
    } catch (error) {
      status.textContent = error.message;
    }
  });
  byId("close-client-settings").addEventListener("click", () => dialog.close());
  byId("client-app-passwords").addEventListener("click", () => {
    dialog.close();
    byId("show-app-passwords").click();
  });
  copy.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(clipboard);
      status.textContent = "已复制客户端配置。";
    } catch {
      status.textContent = "请选中配置后手动复制。";
    }
  });
}
