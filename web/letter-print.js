"use strict";

const time = document.querySelector("time");
const date = new Date(time.dateTime);
if (!Number.isNaN(date.valueOf())) {
  time.textContent = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" }).format(date);
}
document.getElementById("print").addEventListener("click", () => window.print());
if (document.body.dataset.autoPrint === "true") {
  window.addEventListener("load", () => window.print(), { once: true });
}
