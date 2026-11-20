"use strict";

const forwardDialog = document.getElementById("forward-dialog");
const forwardForm = document.getElementById("forward-form");
const forwardDivider = "\n\n---------- 转发的邮件 ----------\n";
let forwardSource = null;
let forwardRequest = null;

function canForward(item) {
  return item && !item.delivery && item.folder !== "drafts";
}

function closeForward() {
  forwardRequest?.abort();
  forwardRequest = null;
  forwardSource = null;
  if (forwardDialog.open) forwardDialog.close();
}

function syncForwardOptions() {
  const include = byId("forward-files");
  include.disabled = forwardForm.elements.mode.value !== "inline";
  byId("forward-files-option").hidden = include.disabled || !forwardSource?.attachments?.length;
  byId("forward-error").textContent = "";
}

function openForward() {
  const item = state.current;
  if (!canForward(item) || editor.open) return;
  forwardSource = item;
  const htmlOnly = item.hasHTML && !item.hasText;
  forwardForm.elements.mode.value = htmlOnly ? "eml" : "inline";
  forwardForm.querySelector('[value="inline"]').disabled = htmlOnly;
  byId("forward-html-hint").hidden = !htmlOnly;
  byId("forward-files").checked = true;
  setForwardLoading(false);
  syncForwardOptions();
  forwardDialog.showModal();
}

function setForwardLoading(loading) {
  byId("forward-options").disabled = loading;
  const submit = byId("start-forward");
  submit.disabled = loading;
  submit.textContent = loading ? "正在准备…" : "继续写信";
}

function forwardSubject(value) {
  const original = value.replace(/[\r\n]+/g, " ").trim() || "（无主题）";
  const subject = /^fwd?\s*:/i.test(original) ? original : "Fwd: " + original;
  const bytes = new TextEncoder().encode(subject);
  return bytes.length <= 300 ? subject : new TextDecoder().decode(bytes.subarray(0, 300), { stream: true });
}

function forwardQuote(item) {
  const recipients = item.headerRecipients;
  return forwardDivider + [
    "发件人：" + item.from,
    ...(recipients?.to ? ["收件人：" + recipients.to] : []),
    ...(recipients?.cc ? ["抄送：" + recipients.cc] : []),
    "时间：" + fullDate.format(item.date * 1000), "主题：" + (item.subject || "（无主题）"),
    "", item.body].join("\n");
}

async function loadForwardOriginal(item, signal) {
  const response = await fetch("/api/letters/" + item.id + "/raw", { signal });
  if (!response.ok) throw new Error("无法读取原邮件，请刷新后重试。");
  const blob = await response.blob();
  if (blob.size > attachmentSizeLimit) {
    const alternatives = item.hasHTML && !item.hasText ? "可从阅读区下载原邮件。" : "可以改用正文引用。";
    throw new Error("原邮件超过 " + attachmentSizeLimit / 1048576 + " MiB 的附件上限。" + alternatives);
  }
  return new File([blob], "原邮件.eml");
}

document.getElementById("forward").addEventListener("click", openForward);
document.getElementById("close-forward").addEventListener("click", closeForward);
forwardForm.addEventListener("change", syncForwardOptions);
forwardDialog.addEventListener("cancel", event => {
  event.preventDefault();
  closeForward();
});
forwardForm.addEventListener("submit", async event => {
  event.preventDefault();
  const item = forwardSource;
  if (!item || forwardRequest) return;
  const request = new AbortController();
  forwardRequest = request;
  const original = forwardForm.elements.mode.value === "eml";
  const includeFiles = byId("forward-files").checked;
  byId("forward-error").textContent = "";
  setForwardLoading(true);
  try {
    let files = [];
    if (original) files = [await loadForwardOriginal(item, request.signal)];
    else if (includeFiles) files = await loadLetterAttachments(item, request.signal);
    if (forwardRequest !== request || state.current?.id !== item.id || editor.open) return;
    closeForward();
    const body = original ? "原邮件见附件。" : forwardQuote(item);
    if (!(await compose(null, { mode: "forward", body, quote: !original, source: item,
      subject: forwardSubject(item.subject), files }))) return;
    if (new Blob([editorForm.elements.body.value]).size > 256 * 1024) {
      byId("send-error").textContent = "引用后的正文超过 256 KiB，请删减后再保存或发送。";
    }
  } catch (error) {
    if (forwardRequest === request) {
      byId("forward-error").textContent = error instanceof TypeError ? "无法读取原邮件，请检查连接后重试。" : error.message;
    }
  } finally {
    if (forwardRequest === request) {
      forwardRequest = null;
      setForwardLoading(false);
    }
  }
});
