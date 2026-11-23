"use strict";

function displayMailSender(item) {
  if (item.senderName && item.senderAddress) return item.senderName + " <" + item.senderAddress + ">";
  return item.senderAddress || item.from || "退信";
}

const senderDetails = document.getElementById("sender-details");
const senderHeaders = document.getElementById("sender-detail-headers");
const senderStatus = document.getElementById("sender-detail-status");
const unsubscribeLetter = document.getElementById("unsubscribe-letter");
let senderRequest;
let senderHeadersLoaded = false;

function resetSenderDetails() {
  senderRequest?.abort();
  senderRequest = undefined;
  senderHeadersLoaded = false;
  senderDetails.open = false;
  senderHeaders.hidden = true;
  senderHeaders.replaceChildren();
  senderStatus.textContent = "";
  unsubscribeLetter.hidden = true;
  unsubscribeLetter.removeAttribute("href");
}

function renderSenderSummary(item) {
  const from = item.from || "退信";
  const senderName = item.senderName || item.senderAddress || from;
  document.getElementById("sender-summary").textContent = senderName;
  const address = document.getElementById("sender-address");
  address.hidden = !item.senderAddress || senderName === item.senderAddress;
  address.textContent = address.hidden ? "" : "<" + item.senderAddress + ">";
  const ownAddress = document.getElementById("account").textContent.toLowerCase();
  const recipient = item.to?.trim().toLowerCase() === ownAddress ? "我" : item.to;
  document.getElementById("recipient-summary").textContent = recipient ? "发送至 " + recipient : "邮件详情";
  senderDetails.querySelector("summary").setAttribute("aria-label", "查看邮件详情，发件人 " + senderName);
  document.getElementById("sender-detail-subject").textContent = item.subject || "（无主题）";
  void loadSenderHeaders(item);
}

function renderLetterAddresses(item) {
  const sender = item.senderAddress || (item.delivery ? item.from : "");
  for (const [id, address, label] of [
    ["sender", sender, "发件人"],
    ["recipient", item.headerRecipients?.to || item.to, "收件人"],
    ["letter-cc", item.headerRecipients?.cc || item.cc, "抄送"],
    ["letter-bcc", item.bcc, "密送"]
  ]) {
    const field = byId(id);
    const heading = byId(id + "-label");
    if (heading) heading.hidden = field.hidden = !address;
    if (!address) {
      field.textContent = id === "sender" ? item.from || "退信" : "";
      continue;
    }
    const value = document.createElement("span");
    value.textContent = address;
    const name = id === "sender" && item.senderName !== address ? item.senderName : "";
    const button = document.createElement("button");
    button.type = "button";
    button.className = "copy-letter-address";
    button.textContent = "复制";
    button.setAttribute("aria-label", "复制" + label + "地址");
    button.addEventListener("click", async () => {
      const selectAddress = () => {
        const range = document.createRange();
        range.selectNodeContents(value);
        const selection = window.getSelection();
        selection.removeAllRanges();
        selection.addRange(range);
        return selection;
      };
      try {
        if (navigator.clipboard) {
          await navigator.clipboard.writeText(address);
        } else {
          const selection = selectAddress();
          if (!document.execCommand("copy")) throw new Error();
          selection.removeAllRanges();
        }
        toast("已复制" + label + "地址");
      } catch {
        if (!value.isConnected) return;
        selectAddress();
        toast("已选中地址，请手动复制");
      }
    });
    field.replaceChildren(name ? name + " " : "", value, " ", button);
  }
}

async function loadSenderHeaders(item) {
  if (!item || item.delivery || senderHeadersLoaded || senderRequest) return;
  const request = new AbortController();
  senderRequest = request;
  senderStatus.textContent = "正在读取邮件头…";
  try {
    const data = await api("letters/" + item.id + "/sender-details", "GET", undefined, {}, request.signal);
    if (senderRequest !== request) return;
    if (data.unsubscribe) {
      unsubscribeLetter.href = data.unsubscribe;
      unsubscribeLetter.hidden = false;
    }
    const fields = document.createDocumentFragment();
    const replyTo = item.replyAddress !== item.senderAddress ? item.replyAddress : "";
    for (const [name, value] of [
      ["回复地址", replyTo],
      ["退信地址（邮件头）", data.returnPath],
      ["邮件列表", data.listId],
      ["DKIM 声明域", data.signingDomain],
      ["邮件 ID", data.messageId],
      ["传输加密", "暂无可验证记录"]
    ]) {
      if (!value) continue;
      const label = document.createElement("dt");
      const detail = document.createElement("dd");
      label.textContent = name;
      detail.textContent = value;
      fields.append(label, detail);
    }
    senderHeaders.replaceChildren(fields);
    senderHeaders.hidden = false;
    senderHeadersLoaded = true;
    senderStatus.textContent = "";
  } catch (error) {
    if (senderRequest === request) senderStatus.textContent = error.message;
  } finally {
    if (senderRequest === request) senderRequest = undefined;
  }
}

senderDetails.addEventListener("toggle", () => {
  if (senderDetails.open) void loadSenderHeaders(state.current);
});
