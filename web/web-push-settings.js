"use strict";

{
  const button = document.getElementById("web-push-toggle");
  const status = document.getElementById("web-push-status");
  window.refreshWebPushPreference = refresh;
  let preference;

  function supported() {
    return isSecureContext && "serviceWorker" in navigator && "PushManager" in window && "Notification" in window;
  }

  async function refresh() {
    button.disabled = true;
    status.textContent = "正在读取通知设置…";
    try {
      preference = await api("web-push");
      const enabled = preference.enabled;
      button.textContent = enabled ? "关闭本设备通知" : "开启本设备通知";
      if (!supported()) {
        status.textContent = "当前环境不支持推送。请使用 HTTPS 和支持通知的浏览器。iPhone 和 iPad 请先将邮箱加入主屏幕。";
      } else if (!preference.available) {
        status.textContent = "服务器暂未提供新邮件通知。";
      } else if (Notification.permission === "denied") {
        status.textContent = "浏览器已阻止通知，可在此站点的权限设置中允许。";
      } else {
        status.textContent = enabled ? "已开启。通知不会显示发件人、主题或正文。" : "开启后，关闭邮箱标签页也能收到收件箱的新邮件提醒。";
      }
      button.disabled = !(enabled || (supported() && preference.available && Notification.permission !== "denied"));
    } catch (error) {
      status.textContent = error.message;
    }
  }

  button.addEventListener("click", async () => {
    button.disabled = true;
    let created;
    try {
      if (preference.enabled) {
        await api("web-push", "DELETE");
        if (supported()) {
          const registration = await navigator.serviceWorker.getRegistration();
          const subscription = await registration?.pushManager.getSubscription();
          if (subscription) await subscription.unsubscribe().catch(() => {});
        }
      } else {
        if (await Notification.requestPermission() !== "granted") {
          await refresh();
          return;
        }
        await navigator.serviceWorker.register("/web-push-worker.js");
        const registration = await navigator.serviceWorker.ready;
        const key = Uint8Array.from(atob(preference.publicKey.replace(/-/g, "+").replace(/_/g, "/")), char => char.charCodeAt(0));
        let subscription = await registration.pushManager.getSubscription();
        if (subscription && !new Uint8Array(subscription.options.applicationServerKey).every((byte, i) => key[i] === byte)) {
          await subscription.unsubscribe();
          subscription = null;
        }
        if (!subscription) {
          created = await registration.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: key });
          subscription = created;
        }
        await api("web-push", "PUT", subscription.toJSON());
      }
      await refresh();
    } catch (error) {
      if (created) await created.unsubscribe().catch(() => {});
      status.textContent = error.message || "暂时无法保存通知设置，请重试。";
      button.disabled = false;
    }
  });
}
