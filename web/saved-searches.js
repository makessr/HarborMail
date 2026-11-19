"use strict";

const savedSearches = new Map();
let savedSearchListRequest = 0;
let savedSearchDialogRequest = 0;
let editingSearch = null;

function syncSavedSearchNavigation() {
  for (const button of byId("saved-search-nav").children) {
    button.setAttribute("aria-current", String(Number(button.dataset.savedSearch) === state.savedSearch?.id));
  }
}

function renderSavedSearchNavigation() {
  const buttons = Array.from(savedSearches.values(), item => {
    const button = document.createElement("button");
    const label = document.createElement("span");
    label.className = "folder-label";
    label.textContent = item.name;
    button.dataset.savedSearch = item.id;
    button.title = item.unavailable ? "重新选择「" + item.name + "」的搜索范围" : item.name;
    button.append(createIcon("search-eye-line"), label);
    return button;
  });
  byId("saved-search-nav").replaceChildren(...buttons);
  syncSavedSearchNavigation();
}

function removeActiveSavedSearch(id) {
  if (state.savedSearch?.id !== id) return;
  state.savedSearch = null;
  updateFolderHeading();
  syncMailboxNavigation();
}

function resetSavedSearches() {
  ++savedSearchListRequest;
  state.savedSearch = null;
  savedSearches.clear();
  byId("saved-search-nav").replaceChildren();
  closeSavedSearchDialog();
}

async function refreshSavedSearches() {
  const request = ++savedSearchListRequest;
  try {
    const items = await api("saved-searches");
    if (request !== savedSearchListRequest) return;
    if (items.length === savedSearches.size && items.every(item => {
      const old = savedSearches.get(item.id);
      return old?.revision === item.revision && old.unavailable === item.unavailable;
    })) return;
    savedSearches.clear();
    for (const item of items) savedSearches.set(item.id, item);
    renderSavedSearchNavigation();
    if (state.savedSearch && !savedSearches.has(state.savedSearch.id)) removeActiveSavedSearch(state.savedSearch.id);
  } catch {
    // Keep the current shortcuts for transient background failures.
  }
}

function openSavedSearch(item) {
  if (item.unavailable) {
    openSavedSearchDialog(item, true);
    return;
  }
  return selectFolder(item.folder, item.label, item);
}

function appliedSearch() {
  return { folder: state.folder, label: state.label, query: byId("search").value.trim(), filters: { ...state.filterValues } };
}

function renderSearchSummary({ query, filters }) {
  const conditions = [
    ["关键词", query], ["发件人", filters.sender], ["收件地址", filters.recipient],
    ["关键词范围", filters.content === "1" ? "仅主题和正文" : ""],
    ["阅读状态", { read: "已读", unread: "未读" }[filters.seen]],
    ["开始日期", filters.start], ["结束日期", filters.end],
  ].filter(([, value]) => value);
  if (!conditions.length) conditions.push(["条件", "显示所选范围内的所有邮件"]);
  byId("saved-search-summary").replaceChildren(...conditions.flatMap(([title, value]) => {
    const term = document.createElement("dt"), detail = document.createElement("dd");
    term.textContent = title;
    detail.textContent = value;
    return [term, detail];
  }));
}

function openSavedSearchDialog(item = state.savedSearch, recover = false) {
  editingSearch = { ...item, ...(recover ? item : appliedSearch()) };
  const scope = byId("saved-search-scope");
  const hasFilters = Object.keys(editingSearch.filters).length !== 0;
  const builtinOptions = Object.entries(folders)
    .filter(([folder]) => folder !== "outbox" || !hasFilters)
    .map(([folder, name]) => new Option(name, folder));
  scope.replaceChildren(new Option("选择搜索范围", ""), ...builtinOptions,
    ...Array.from(customFolders, ([id, name]) => new Option("文件夹「" + name + "」", id)),
    ...Array.from(mailLabels.values(), ({ id, name }) => new Option("标签「" + name + "」", "label:" + id)));
  scope.value = recover ? "" : editingSearch.label ? "label:" + editingSearch.label : editingSearch.folder;
  byId("saved-search-name").value = item?.name || "";
  byId("saved-search-title").textContent = item ? "编辑搜索" : "保存搜索";
  byId("submit-saved-search").textContent = item ? "更新搜索" : "保存搜索";
  byId("delete-saved-search").hidden = !item;
  byId("saved-search-notice").hidden = !recover;
  byId("saved-search-error").textContent = "";
  renderSearchSummary(editingSearch);
  byId("saved-search-dialog").showModal();
  byId("saved-search-name").focus();
}

function closeSavedSearchDialog() {
  ++savedSearchDialogRequest;
  editingSearch = null;
  byId("saved-search-dialog").close();
  byId("saved-search-form").reset();
  byId("saved-search-summary").replaceChildren();
}

async function changeSavedSearch(operation) {
  const controls = ["close-saved-search", "submit-saved-search", "delete-saved-search", "saved-search-name", "saved-search-scope"].map(byId);
  if (!editingSearch || controls[1].disabled) return;
  const request = savedSearchDialogRequest;
  for (const control of controls) control.disabled = true;
  byId("saved-search-error").textContent = "";
  try {
    const update = await operation(editingSearch);
    if (request !== savedSearchDialogRequest) return;
    ++savedSearchListRequest;
    if (update) {
      savedSearches.set(update.id, update);
      openSavedSearch(update);
    } else {
      savedSearches.delete(editingSearch.id);
      removeActiveSavedSearch(editingSearch.id);
    }
    renderSavedSearchNavigation();
    closeSavedSearchDialog();
    toast(update ? "搜索已保存" : "搜索已删除");
    void refreshSavedSearches();
  } catch (error) {
    if (request === savedSearchDialogRequest) byId("saved-search-error").textContent = error.message;
  } finally {
    for (const control of controls) control.disabled = false;
  }
}

document.addEventListener("DOMContentLoaded", () => {
  byId("saved-search-nav").addEventListener("click", event => {
    const button = event.target.closest("[data-saved-search]");
    const item = savedSearches.get(Number(button?.dataset.savedSearch));
    if (item) openSavedSearch(item);
  });
  byId("save-search").addEventListener("click", () => openSavedSearchDialog());
  byId("close-saved-search").addEventListener("click", closeSavedSearchDialog);
  byId("saved-search-dialog").addEventListener("cancel", event => {
    event.preventDefault();
    if (!byId("close-saved-search").disabled) closeSavedSearchDialog();
  });
  byId("saved-search-form").addEventListener("submit", event => {
    event.preventDefault();
    const name = byId("saved-search-name").value;
    const scope = byId("saved-search-scope").value;
    const tagged = scope.startsWith("label:");
    void changeSavedSearch(({ id, revision, query, filters }) => api(
      id ? "saved-searches/" + id : "saved-searches", id ? "PUT" : "POST",
      { revision, name, query, filters, folder: tagged ? "all" : scope, label: tagged ? Number(scope.slice(6)) : 0 },
    ));
  });
  byId("delete-saved-search").addEventListener("click", () => {
    if (!editingSearch || !confirm("删除搜索「" + editingSearch.name + "」？")) return;
    void changeSavedSearch(({ id, revision }) => api("saved-searches/" + id, "DELETE", undefined, { "If-Match": String(revision) }));
  });
});
