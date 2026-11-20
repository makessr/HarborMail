"use strict";

let undoNotice = null;
let undoTicker;
let outboxUndoTimer;

function localScheduleValue(date) {
  return new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
}

function setScheduleVisible(visible) {
  const time = byId("schedule-time");
  byId("schedule-fields").hidden = !visible;
  time.disabled = !visible;
  byId("toggle-schedule").setAttribute("aria-expanded", String(visible));
  byId("send-action-label").textContent = visible ? "确认定时发送" : "发送邮件";
  updateComposeUndoHint();
  if (visible) {
    time.min = localScheduleValue(new Date(Date.now() + 60000));
    if (!time.value) time.value = localScheduleValue(new Date(Date.now() + 3600000));
    const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
    byId("schedule-zone").textContent = zone ? "使用设备时区（" + zone + "）" : "使用设备时区";
    time.focus();
    byId("send-action-label").closest("button").scrollIntoView({ block: "nearest" });
  }
}

function updateComposeUndoHint() {
  const hint = byId("compose-undo-hint");
  const seconds = activeMailboxPreferences.undoSeconds;
  hint.hidden = !seconds || !byId("schedule-time").disabled;
  hint.textContent = hint.hidden ? "" : "发送后有 " + seconds + " 秒可以撤销。";
}

function composeScheduleQuery() {
  const input = byId("schedule-time");
  if (input.disabled) {
    const seconds = activeMailboxPreferences.undoSeconds;
    return seconds ? "?undoSeconds=" + seconds : "";
  }
  const selected = new Date(input.value);
  if (!Number.isFinite(selected.getTime()) || selected.getTime() <= Date.now() || localScheduleValue(selected) !== input.value) {
    throw new Error("请选择有效的未来发送时间");
  }
  return "?sendAt=" + Math.floor(selected.getTime() / 1000);
}

function trackOutboxUndo(item) {
  const status = item.delivery;
  if (status?.state === "undo") {
    const remaining = status.undoRemainingMs ?? status.due * 1000 - Date.now();
    status.undoExpires = performance.now() + Math.max(0, remaining);
  }
}

function canUndoOutbox(status) {
  return status?.state === "undo" && status.undoExpires > performance.now();
}

function outboxStatusLabel(status) {
  return deliveryLabels[status.state === "undo" && !canUndoOutbox(status) ? "queued" : status.state];
}

function displayOutboxStatus(item) {
  const status = item.delivery;
  const undo = canUndoOutbox(status);
  const planned = status.state === "undo" ? 0 : status.sendAt;
  const parts = [outboxStatusLabel(status)];
  if (status.reason) parts.push(status.reason);
  if (status.state === "queued" && status.attempts > 0) {
    parts.push("下次尝试 " + fullDate.format(status.due * 1000));
  }
  byId("delivery-status").textContent = parts.join(" ");
  byId("letter-date-label").textContent = undo ? "撤销截止" : planned ? "计划时间" : "时间";
  byId("date").textContent = fullDate.format((undo ? status.due : planned || item.date) * 1000);
  const cancel = byId("cancel-scheduled");
  const hadFocus = document.activeElement === cancel;
  cancel.hidden = status.state !== "scheduled" && !undo;
  cancel.textContent = status.state === "undo" ? "撤销发送" : "取消并转为草稿";
  if (cancel.hidden && hadFocus) byId("subject").focus({ preventScroll: true });
}

let recipientProgressRequest = 0;
const recipientProgressLabels = {
  queued: "等待重试", sending: "投递中", accepted: "中继已接收", failed: "失败", uncertain: "结果待确认"
};

function renderDeliveryTimeline(events) {
  const details = byId("delivery-timeline");
  details.hidden = !events?.length;
  const labels = { ...recipientProgressLabels, enqueued: "加入发件箱", local_delivered: "站内投递完成",
    sent: "外部投递完成", retry_requested: "管理员安排重试" };
  byId("delivery-timeline-items").replaceChildren(...(events || []).map(({state, address, at, due, code}) => {
    const row = document.createElement("li");
    const fields = [fullDate.format(at * 1000), labels[state] || state, address];
    if (code) fields.push("SMTP " + code);
    if (due && state !== "enqueued") fields.push("下次尝试 " + fullDate.format(due * 1000));
    row.textContent = fields.filter(Boolean).join(" · ");
    return row;
  }));
}

function renderOutboxRecipients(delivery) {
  const details = byId("delivery-recipients");
  const items = delivery?.recipients || [];
  details.hidden = items.length === 0;
  const list = byId("delivery-recipient-items");
  list.replaceChildren(...items.map(({ address, state: status, attempts, due, code }) => {
    const row = document.createElement("li");
    const parts = [address, recipientProgressLabels[status] || status, attempts + " 次"];
    if (code) parts.push("SMTP " + code);
    if (status === "queued" && due) parts.push("下次尝试 " + fullDate.format(due * 1000));
    row.textContent = parts.join(" · ");
    if (status === "uncertain" && delivery.state === "uncertain") {
      for (const [outcome, label] of [["accepted", "确认已接收"], ["failed", "确认未接收"]]) {
        const button = document.createElement("button");
        button.type = "button";
        button.className = "secondary";
        button.textContent = label;
        button.addEventListener("click", () => void confirmRecipientResult(button, address, outcome));
        row.append(" ", button);
      }
    }
    return row;
  }));
}

async function refreshOutboxRecipients(id) {
  const request = ++recipientProgressRequest;
  const delivery = state.current?.delivery;
  if (!delivery || !delivery.attempts) return;
  try {
    const item = await api("outbox/" + id);
    if (request !== recipientProgressRequest || state.current?.id !== id || !state.current.delivery) return;
    state.current.delivery.recipients = item.delivery.recipients;
    renderOutboxRecipients(state.current.delivery);
    renderDeliveryTimeline(item.timeline);
  } catch (error) {
    if (error.status !== 404 && request === recipientProgressRequest && state.current?.id === id) toast(error.message);
  }
}

async function confirmRecipientResult(button, address, outcome) {
  const current = state.current;
  if (!current?.delivery || !confirm("已核对中继记录，确认 " + address + " 的投递结果？邮件不会重发。")) return;
  button.disabled = true;
  try {
    await api("outbox/" + current.id + "/recipient-result", "POST", { address, outcome });
    toast("已记录这个地址的投递结果。");
    await load();
  } catch (error) {
    toast(error.message);
  } finally {
    button.disabled = false;
  }
}

function refreshOutboxUndoExpiry() {
  clearTimeout(outboxUndoTimer);
  if (state.folder !== "outbox" || byId("mailbox").hidden) return;
  const now = performance.now();
  let next = Infinity;
  for (const row of byId("messages").querySelectorAll("[data-undo-expires]")) {
    const expires = Number(row.dataset.undoExpires);
    if (expires <= now) {
      row.querySelector(".row-preview").textContent = deliveryLabels.queued;
      delete row.dataset.undoExpires;
    } else {
      next = Math.min(next, expires);
    }
  }
  const status = state.current?.delivery;
  if (status?.state === "undo") {
    displayOutboxStatus(state.current);
    if (status.undoExpires > now) next = Math.min(next, status.undoExpires);
  }
  if (Number.isFinite(next)) outboxUndoTimer = setTimeout(refreshOutboxUndoExpiry, Math.ceil(Math.max(0, next - performance.now())));
}

function resetUndoSend() {
  clearInterval(undoTicker);
  undoNotice = null;
  byId("undo-send-notice").hidden = true;
}

function refreshUndoNotice() {
  if (!undoNotice) return;
  const seconds = Math.ceil((undoNotice.expires - performance.now()) / 1000);
  if (seconds <= 0) {
    resetUndoSend();
    return;
  }
  byId("undo-send-countdown").textContent = "剩余 " + seconds + " 秒";
}

function showUndoSend(result) {
  resetUndoSend();
  if (result.undoRemainingMs <= 0) return;
  undoNotice = { id: result.queued, expires: performance.now() + result.undoRemainingMs };
  byId("undo-send-action").disabled = false;
  byId("undo-send-notice").hidden = false;
  refreshUndoNotice();
  if (undoNotice) undoTicker = setInterval(refreshUndoNotice, 200);
}

async function cancelPendingSend(id, button, undo, fromNotice = false) {
  const session = preferencesSession;
  button.disabled = true;
  try {
    const { draft } = await api("outbox/" + id + "/cancel", "POST");
    if (session !== preferencesSession) return;
    if (undoNotice?.id === id) resetUndoSend();
    toast(undo ? "已撤销发送，邮件已转为草稿" : "已取消定时发送，邮件已转为草稿");
    if ((fromNotice || (state.folder === "outbox" && state.current?.id === id)) && !document.querySelector("dialog[open]")) {
      selectFolder("drafts");
      await openLetter(draft);
      if (undo && session === preferencesSession && state.folder === "drafts" && state.current?.id === draft) {
        await resumeDraft();
      }
    } else if (state.folder === "outbox") {
      await load(false, true);
    }
  } catch (error) {
    if (session !== preferencesSession) return;
    if (undo && (error.status === 404 || error.status === 409)) {
      if (undoNotice?.id === id) resetUndoSend();
      toast("撤销期限已过或邮件已开始投递");
    } else {
      toast(error.message);
    }
    if (state.folder === "outbox") await load(false, true);
  } finally {
    button.disabled = false;
  }
}

document.getElementById("toggle-schedule").addEventListener("click", () => {
  setScheduleVisible(byId("schedule-fields").hidden);
});

document.getElementById("cancel-scheduled").addEventListener("click", event => {
  const item = state.current;
  if (item?.delivery?.state === "scheduled" || canUndoOutbox(item?.delivery)) {
    void cancelPendingSend(item.id, event.currentTarget, item.delivery.state === "undo");
  }
});

document.addEventListener("visibilitychange", () => {
  if (document.hidden) return;
  refreshUndoNotice();
  refreshOutboxUndoExpiry();
});

document.getElementById("undo-send-action").addEventListener("click", event => {
  refreshUndoNotice();
  if (undoNotice) void cancelPendingSend(undoNotice.id, event.currentTarget, true, true);
});
