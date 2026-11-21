"use strict";

let adminFeedbackQueue = Promise.resolve();

function adminFeedback(message, kind = "success", confirmLabel = "") {
  const show = () => new Promise(resolve => {
    const dialog = document.getElementById("admin-feedback");
    const cancel = document.getElementById("admin-feedback-cancel");
    const ok = document.getElementById("admin-feedback-ok");
    dialog.dataset.kind = kind;
    dialog.returnValue = "";
    document.getElementById("admin-feedback-title").textContent = {
      success: "操作完成", error: "操作未完成", confirm: "确认操作", info: "管理员交接"
    }[kind];
    document.getElementById("admin-feedback-message").textContent = message;
    dialog.querySelector("use").setAttribute("href", "/icons.svg#" + (kind === "success" ? "checkbox-fill" : "spam-2-line"));
    cancel.hidden = !confirmLabel;
    ok.textContent = confirmLabel || "确定";
    dialog.addEventListener("close", () => resolve(dialog.returnValue === "ok"), { once: true });
    dialog.showModal();
    (confirmLabel ? cancel : ok).focus();
  });
  adminFeedbackQueue = adminFeedbackQueue.then(show);
  return adminFeedbackQueue;
}
