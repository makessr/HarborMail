"use strict";

const mailLabels = new Map();
let labelListRequest = 0;
let labelMembershipRequest = 0;
let labelPicker = false;
let editingLabel = 0;
let labelPending = false;

function syncLabelNavigation() {
  for (const button of byId("label-folders").children) {
    button.setAttribute("aria-current", String(!state.savedSearch && Number(button.dataset.label) === state.label));
  }
}

function renderLabelNavigation() {
  const buttons = Array.from(mailLabels.values(), ({ id, name }) => {
    const button = document.createElement("button");
    const text = document.createElement("span");
    text.className = "folder-label";
    text.textContent = name;
    button.dataset.label = id;
    button.append(createIcon("price-tag-3-line"), text);
    return button;
  });
  byId("label-folders").replaceChildren(...buttons);
  syncLabelNavigation();
}

function resetMailLabels() {
  ++labelListRequest;
  ++labelMembershipRequest;
  mailLabels.clear();
  byId("label-folders").replaceChildren();
  byId("labels-dialog").close();
}

async function refreshMailLabels(showError = false) {
  const request = ++labelListRequest;
  try {
    const labels = await api("labels");
    if (request !== labelListRequest) return;
    if (labels.length === mailLabels.size && labels.every(({ id, name }) => mailLabels.get(id)?.name === name)) return;
    mailLabels.clear();
    for (const label of labels) mailLabels.set(label.id, label);
    renderLabelNavigation();
    if (state.label && !mailLabels.has(state.label)) selectFolder("inbox");
    else if (state.label) updateFolderHeading();
    else renderBatchOptions();
    if (state.current?.labels?.length) {
      state.current.labels = state.current.labels.map(label => mailLabels.get(label.id)).filter(Boolean);
      renderLetterLabels(state.current);
    }
    if (editingLabel && !mailLabels.has(editingLabel)) editLabel();
    if (byId("labels-dialog").open) renderLabelOptions();
  } catch (error) {
    if (showError && request === labelListRequest) byId("labels-error").textContent = error.message;
  }
}

function canLabel(item) {
  return item && !item.delivery && item.folder !== "drafts";
}

function renderLetterLabels(item) {
  const active = canLabel(item);
  byId("letter-labels").hidden = !active;
  if (!active) return;
  const chips = (item.labels || []).map(({ name }) => {
    const chip = document.createElement("li");
    chip.textContent = name;
    return chip;
  });
  byId("letter-label-list").replaceChildren(...chips);
  byId("letter-label-action").textContent = chips.length ? "编辑标签" : "添加标签";
}

async function refreshLetterLabels(item, showError = false) {
  if (!canLabel(item)) return;
  const request = ++labelMembershipRequest;
  try {
    const labels = await api("letters/" + item.id + "/labels");
    if (request !== labelMembershipRequest || item !== state.current) return;
    item.labels = labels;
    renderLetterLabels(item);
    if (state.label && !labels.some(label => label.id === state.label)) clearReading();
    if (labelPicker && byId("labels-dialog").open) syncLabelChecks();
  } catch (error) {
    if (showError && request === labelMembershipRequest) byId("labels-error").textContent = error.message;
  }
}

function syncLabelChecks() {
  const checked = new Set((state.current?.labels || []).map(label => label.id));
  for (const input of byId("labels-list").querySelectorAll("input")) {
    if (!input.disabled) input.checked = checked.has(Number(input.value));
  }
}

function editLabel(label) {
  editingLabel = label?.id || 0;
  byId("label-form").elements.name.value = label?.name || "";
  byId("save-label").textContent = label ? "保存名称" : "新建标签";
  byId("cancel-label-edit").hidden = !label;
  if (label) byId("label-form").elements.name.focus();
}

function renderLabelOptions() {
  const list = byId("labels-list");
  const items = Array.from(mailLabels.values(), label => {
    const item = document.createElement("li");
    if (labelPicker) {
      const option = document.createElement("label");
      const input = document.createElement("input");
      input.type = "checkbox";
      input.value = label.id;
      input.addEventListener("change", () => void toggleLetterLabel(input));
      option.append(input, label.name);
      item.append(option);
    } else {
      const name = document.createElement("span");
      name.textContent = label.name;
      const rename = document.createElement("button");
      rename.type = "button";
      rename.className = "secondary";
      rename.textContent = "重命名";
      rename.setAttribute("aria-label", "重命名标签「" + label.name + "」");
      rename.addEventListener("click", () => editLabel(label));
      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "secondary";
      remove.textContent = "删除";
      remove.setAttribute("aria-label", "删除标签「" + label.name + "」");
      remove.addEventListener("click", () => void removeMailLabel(label, remove));
      item.append(name, rename, remove);
    }
    return item;
  });
  if (!items.length) {
    const empty = document.createElement("li");
    empty.className = "label-empty";
    empty.textContent = "暂无标签，可在下方新建。";
    items.push(empty);
  }
  list.replaceChildren(...items);
  if (labelPicker) syncLabelChecks();
}

function openLabels(assign) {
  labelPicker = assign;
  byId("labels-title").textContent = assign ? "编辑邮件标签" : "管理标签";
  byId("labels-error").textContent = "";
  editLabel();
  renderLabelOptions();
  byId("labels-dialog").showModal();
  void refreshMailLabels(true);
}

function closeLabelPicker() {
  if (labelPicker) byId("labels-dialog").close();
}

async function toggleLetterLabel(input) {
  const item = state.current;
  if (!canLabel(item)) return;
  const checked = input.checked;
  input.disabled = true;
  byId("labels-error").textContent = "";
  try {
    await api("letters/" + item.id + "/labels/" + input.value, checked ? "PUT" : "DELETE");
  } catch (error) {
    if (item === state.current) {
      input.checked = (item.labels || []).some(label => label.id === Number(input.value));
      byId("labels-error").textContent = error.message;
    }
  } finally {
    input.disabled = false;
    if (item === state.current) await refreshLetterLabels(item, true);
  }
}

async function removeMailLabel(label, button) {
  if (labelPending || !confirm("删除标签「" + label.name + "」？关联邮件仍会保留。")) return;
  labelPending = true;
  button.disabled = true;
  byId("labels-error").textContent = "";
  try {
    await api("labels/" + label.id, "DELETE");
    if (editingLabel === label.id) editLabel();
    await refreshMailLabels(true);
  } catch (error) {
    byId("labels-error").textContent = error.message;
  } finally {
    labelPending = false;
    button.disabled = false;
  }
}

document.addEventListener("DOMContentLoaded", () => {
  const form = byId("label-form");
  byId("manage-labels").addEventListener("click", () => openLabels(false));
  byId("edit-letter-labels").addEventListener("click", () => openLabels(true));
  byId("close-labels").addEventListener("click", () => byId("labels-dialog").close());
  byId("cancel-label-edit").addEventListener("click", () => editLabel());
  byId("label-folders").addEventListener("click", event => {
    const button = event.target.closest("[data-label]");
    if (button) selectFolder("all", Number(button.dataset.label));
  });
  form.addEventListener("submit", async event => {
    event.preventDefault();
    if (labelPending) return;
    const id = editingLabel;
    const save = byId("save-label");
    labelPending = save.disabled = true;
    byId("labels-error").textContent = "";
    try {
      await api(id ? "labels/" + id : "labels", id ? "PUT" : "POST", { name: form.elements.name.value });
      editLabel();
      await refreshMailLabels(true);
    } catch (error) {
      byId("labels-error").textContent = error.message;
    } finally {
      labelPending = save.disabled = false;
    }
  });
});
