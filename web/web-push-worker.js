"use strict";

self.addEventListener("push", event => {
  event.waitUntil(self.registration.showNotification("Harbor Mail", {
    body: "你有一封新邮件",
    icon: "/harbor-mail-logo.png",
    tag: "harbor-inbox",
  }));
});

self.addEventListener("notificationclick", event => {
  event.notification.close();
  event.waitUntil((async () => {
    const pages = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
    const mailbox = pages.find(client => new URL(client.url).pathname === "/");
    if (mailbox) return mailbox.focus();
    return self.clients.openWindow("/?view=mail");
  })());
});
