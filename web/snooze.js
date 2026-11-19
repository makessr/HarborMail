"use strict";

const snoozeDialog = document.getElementById("snooze-dialog");
const snoozeForm = document.getElementById("snooze-form");
let editingSnooze = null;

function renderSnooze(item) {
  const scheduled = Boolean(item.snoozeUntil);
  byId("edit-snooze").hidden = Boolean(item.delivery) || (item.folder !== "inbox" && !scheduled);
  byId("edit-snooze-label").textContent = scheduled ? "管理稍后处理" : "稍后再看";
  byId("snooze-status").hidden = !scheduled;
  byId("snooze-status-text").textContent = scheduled ? "将于 " + fullDate.format(item.snoozeUntil * 1000) + " 移回收件箱" : "";
}

function snoozePreset(name) {
  const date = new Date(Date.now() + 3600000);
  if (name === "tomorrow") {
    date.setTime(Date.now());
    date.setDate(date.getDate() + 1);
    date.setHours(9, 0, 0, 0);
  }
  return localScheduleValue(date);
}

function openSnooze() {
  const item = state.current;
  if (!item || item.delivery || (item.folder !== "inbox" && !item.snoozeUntil)) return;
  editingSnooze = { id: item.id, previousAt: item.snoozeUntil || 0 };
  const input = byId("snooze-time");
  input.min = localScheduleValue(new Date(Date.now() + 60000));
  input.value = editingSnooze.previousAt ? localScheduleValue(new Date(editingSnooze.previousAt * 1000)) : snoozePreset("hour");
  byId("snooze-title").textContent = editingSnooze.previousAt ? "管理稍后处理" : "稍后处理";
  byId("snooze-subject").textContent = item.subject || "（无主题）";
  byId("restore-snooze").hidden = !editingSnooze.previousAt;
  byId("save-snooze").textContent = editingSnooze.previousAt ? "更新时间" : "稍后再看";
  const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  byId("snooze-zone").textContent = zone ? "使用设备时区（" + zone + "）" : "使用设备时区";
  byId("snooze-error").textContent = "";
  snoozeDialog.showModal();
}

function closeSnooze() {
  editingSnooze = null;
  if (snoozeDialog.open) snoozeDialog.close();
  byId("snooze-subject").textContent = "";
}

async function submitSnooze(restore = false) {
  const editing = editingSnooze;
  const save = byId("save-snooze");
  if (!editing || save.disabled || (restore && !editing.previousAt)) return;
  const input = byId("snooze-time");
  const error = byId("snooze-error");
  let resumeAt = 0;
  if (!restore) {
    const selected = new Date(input.value);
    if (!Number.isFinite(selected.getTime()) || selected.getTime() <= Date.now() || localScheduleValue(selected) !== input.value) {
      error.textContent = "请选择有效的未来时间";
      input.focus();
      return;
    }
    resumeAt = Math.floor(selected.getTime() / 1000);
  }
  const controls = snoozeForm.querySelectorAll("button, input");
  controls.forEach(control => { control.disabled = true; });
  error.textContent = "";
  try {
    await api("letters/" + editing.id + "/snooze", "PUT", { resumeAt, previousAt: editing.previousAt });
    if (editingSnooze !== editing) return;
    closeSnooze();
    if (state.current?.id === editing.id) {
      if (resumeAt && editing.previousAt) renderLetter({ ...state.current, snoozeUntil: resumeAt });
      else clearReading();
    }
    toast(resumeAt ? "将在 " + fullDate.format(resumeAt * 1000) + " 移回收件箱" : "已移回收件箱");
    await load(false, true);
  } catch (failure) {
    if (editingSnooze === editing) error.textContent = failure.message;
  } finally {
    controls.forEach(control => { control.disabled = false; });
  }
}

document.addEventListener("DOMContentLoaded", () => {
  byId("edit-snooze").addEventListener("click", openSnooze);
  byId("close-snooze").addEventListener("click", closeSnooze);
  snoozeDialog.addEventListener("cancel", event => {
    event.preventDefault();
    if (!byId("close-snooze").disabled) closeSnooze();
  });
  for (const button of snoozeForm.querySelectorAll("[data-snooze-preset]")) {
    button.addEventListener("click", () => {
      byId("snooze-time").value = snoozePreset(button.dataset.snoozePreset);
      byId("snooze-error").textContent = "";
    });
  }
  byId("restore-snooze").addEventListener("click", () => void submitSnooze(true));
  snoozeForm.addEventListener("submit", event => {
    event.preventDefault();
    void submitSnooze();
  });
});
