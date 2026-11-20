"use strict";

const byId = (id) => document.getElementById(id);
const svgNamespace = "http://www.w3.org/2000/svg";

function createIcon(name, label = "") {
  const svg = document.createElementNS(svgNamespace, "svg");
  const use = document.createElementNS(svgNamespace, "use");
  svg.setAttribute("class", "glyph");
  svg.setAttribute("focusable", "false");
  if (label) {
    svg.setAttribute("role", "img");
    svg.setAttribute("aria-label", label);
  } else {
    svg.setAttribute("aria-hidden", "true");
  }
  use.setAttribute("href", "/icons.svg#" + name);
  svg.append(use);
  return svg;
}

const state = {
  folder: "inbox", label: 0, savedSearch: null, current: null, next: 0, pages: 0, loading: false, query: "",
  request: 0, reading: 0, folderRequest: 0, htmlMode: false,
  filterValues: {}, batchMode: false, batchPending: false, batchRevision: 0, selected: new Set(),
  conversationRequest: 0, recoveryMode: false,
};
const folders = {
  inbox: "收件箱", snoozed: "稍后处理", all: "全部邮件", starred: "星标", drafts: "草稿箱", outbox: "发件箱", sent: "已发送", archive: "归档",
  junk: "垃圾邮件", trash: "废纸篓",
};
const deliveryLabels = {
  undo: "撤销期内", scheduled: "等待定时发送", queued: "等待投递", preparing: "正在投递", sending: "等待中继确认", failed: "投递失败", uncertain: "结果待确认",
};
const shortDate = new Intl.DateTimeFormat("zh-CN", { month: "numeric", day: "numeric" });
const fullDate = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });
const editor = byId("editor");
const editorForm = byId("editor-form");
const selectedFiles = [];
const attachmentCountLimit = 10;
let attachmentSizeLimit = 6 * 1024 * 1024;
let mailMessageMiB = 10;
const attachmentLimitMessage = () => "每封邮件最多添加 10 个附件，合计不超过 " + attachmentSizeLimit / 1048576 + " MiB。";
const customFolders = new Map();
const folderDialog = byId("folder-dialog");
const folderForm = byId("folder-form");
const rulesDialog = byId("rules-dialog");
const addressesDialog = byId("addresses-dialog");
const filterDialog = byId("filter-dialog");
const filterForm = byId("filter-form");
const batchLimit = 100;
const conversation = byId("conversation");
let replySource = 0;
let composeRequest = 0;
let toastTimeout;
let liveSource;
let liveTimer;
let addressRequest = 0;

async function api(path, method = "GET", data, headers = {}) {
  const options = { method, headers };
  if (data !== undefined) {
    if (data instanceof FormData) {
      options.body = data;
    } else {
      options.headers["Content-Type"] = "application/json";
      options.body = JSON.stringify(data);
    }
  }
  const response = await fetch("/api/" + path, options);
  const payload = response.headers.get("Content-Type")?.includes("application/json")
    ? await response.json() : null;
  if (!response.ok) {
    if (response.status === 401 && path !== "login") showLogin();
    throw Object.assign(new Error(payload?.error || "请求失败，请稍后重试"), { status: response.status });
  }
  return payload;
}

function toast(message) {
  const element = byId("toast");
  element.textContent = message;
  element.hidden = false;
  clearTimeout(toastTimeout);
  toastTimeout = setTimeout(() => { element.hidden = true; }, 5500);
}

function showLogin() {
  liveSource?.close();
  liveSource = null;
  clearTimeout(liveTimer);
  liveTimer = undefined;
  ++state.request;
  ++state.folderRequest;
  ++composeRequest;
  customFolders.clear();
  renderFolderNav();
  resetMailLabels();
  resetSavedSearches();
  resetMailboxPreferences();
  resetUndoSend();
  closeCommands();
  closeSignatures();
  closeMailTemplates();
  resetDraftTracking();
  if (editor.open) editor.close();
  editorForm.reset();
  resetComposeSignatures();
  if (folderDialog.open) folderDialog.close();
  if (rulesDialog.open) rulesDialog.close();
  if (addressesDialog.open) addressesDialog.close();
  if (byId("contacts-dialog").open) byId("contacts-dialog").close();
  byId("app-password-secret").value = "";
  if (byId("app-passwords-dialog").open) byId("app-passwords-dialog").close();
  if (byId("web-sessions-dialog").open) byId("web-sessions-dialog").close();
  if (byId("mail-transfer-dialog").open) byId("mail-transfer-dialog").close();
  if (filterDialog.open) filterDialog.close();
  setBatchMode(false);
  clearReading();
  byId("messages").replaceChildren();
  byId("mailbox").hidden = true;
  byId("admin-link").hidden = true;
  byId("login").hidden = false;
}

async function openSession({ email, role, recoveryMode = false, attachmentMiB = 6, messageMiB = 10 }, keepMailbox = false) {
  state.recoveryMode = recoveryMode;
  attachmentSizeLimit = attachmentMiB * 1048576;
  mailMessageMiB = messageMiB;
  byId("attachment-limits").textContent = attachmentLimitMessage();
  byId("eml-import-limits").textContent = "每批最多 100 封、合计 64 MiB，单封最多 " + mailMessageMiB + " MiB。";
  byId("mail-recovery-mode").hidden = !recoveryMode;
  const url = new URL(location.href);
  const adminDenied = url.searchParams.get("notice") === "admin-forbidden";
  if (adminDenied) {
    url.searchParams.delete("notice");
    history.replaceState(history.state, "", url);
  }
  if (["admin", "domain_admin"].includes(role) && !keepMailbox && !adminDenied) {
    byId("login-form").reset();
    window.location.replace("/admin");
    return;
  }
  byId("admin-link").hidden = !["admin", "domain_admin"].includes(role);
  await showMailbox(email);
  if (adminDenied && !byId("mailbox").hidden) toast("当前账号没有管理权限。");
}

async function showMailbox(email) {
  if (!await refreshMailboxPreferences()) return;
  byId("account").textContent = email;
  byId("account").title = email;
  byId("account-initial").textContent = email[0].toUpperCase();
  byId("login-form").reset();
  byId("login-error").textContent = "";
  editorForm.reset();
  byId("login").hidden = true;
  byId("mailbox").hidden = false;
  selectFolder("inbox");
  void refreshFolders();
  void refreshMailLabels();
  void refreshSavedSearches();
  startMailboxEvents();
}

function startMailboxEvents() {
  liveSource?.close();
  const source = new EventSource("/api/events");
  liveSource = source;
  const refresh = () => {
    if (liveSource !== source || liveTimer !== undefined) return;
    liveTimer = setTimeout(() => {
      liveTimer = undefined;
      if (state.loading) {
        refresh();
        return;
      }
      void refreshFolders();
      void refreshMailLabels();
      void refreshSavedSearches();
      void refreshMailboxPreferences();
      void load(false, true);
      if (state.current) {
        void loadConversation(state.current);
        void refreshLetterLabels(state.current);
      }
    }, 200);
  };
  source.addEventListener("open", refresh);
  source.addEventListener("mailbox", refresh);
  source.addEventListener("session-expired", () => {
    if (liveSource === source) showLogin();
  });
  source.addEventListener("error", () => {
    if (liveSource !== source || source.readyState !== EventSource.CLOSED) return;
    clearTimeout(liveTimer);
    const reconnect = async () => {
      liveTimer = undefined;
      if (liveSource !== source) return;
      try {
        await api("session");
        if (liveSource === source) startMailboxEvents();
      } catch {
        if (liveSource === source) liveTimer = setTimeout(reconnect, 3000);
      }
    };
    liveTimer = setTimeout(reconnect, 3000);
  });
}

function clearReading() {
  closeLabelPicker();
  closeForward();
  closeSnooze();
  ++state.reading;
  state.current = null;
  state.htmlMode = false;
  clearConversation();
  byId("html-frame").removeAttribute("src");
  byId("letter-actions").open = false;
  byId("letter").hidden = true;
  byId("no-selection").hidden = false;
  byId("mailbox").classList.remove("open");
  for (const row of byId("messages").children) row.removeAttribute("aria-current");
}

function selectFolder(folder, label = 0, saved = null, query = "") {
  state.folder = folder;
  state.label = label;
  state.savedSearch = saved;
  state.query = saved?.query ?? query;
  state.filterValues = { ...saved?.filters };
  clearReading();
  const search = byId("search");
  search.value = state.query;
  search.placeholder = label ? "搜索此标签中的邮件" : folder === "all" ? "搜索全部邮件" : "搜索当前文件夹";
  search.setAttribute("aria-label", search.placeholder);
  setBatchMode(false);
  updateFolderHeading();
  syncMailboxNavigation();
  return load();
}

function syncMailboxNavigation() {
  for (const button of document.querySelectorAll("[data-folder]")) {
    button.setAttribute("aria-current", String(!state.savedSearch && !state.label && button.dataset.folder === state.folder));
  }
  syncLabelNavigation();
  syncSavedSearchNavigation();
}

function updateFolderHeading() {
  byId("folder-title").textContent = state.savedSearch?.name || mailLabels.get(state.label)?.name || mailFolderName(state.folder);
  byId("manage-folder").hidden = Boolean(state.savedSearch) || !customFolders.has(state.folder);
  byId("open-filters").hidden = state.folder === "outbox";
  byId("show-mail-transfer").hidden = state.folder === "outbox";
  byId("select-mails").hidden = state.folder === "outbox";
  byId("save-search").textContent = state.savedSearch ? "编辑搜索" : "保存搜索";
  updateFilterButton();
  renderBatchOptions();
}

function renderFolderNav() {
  const container = byId("custom-folders");
  container.replaceChildren();
  for (const [id, name] of customFolders) {
    const button = document.createElement("button");
    button.dataset.folder = id;
    button.setAttribute("aria-current", String(!state.savedSearch && id === state.folder));
    const label = document.createElement("span");
    label.className = "folder-label";
    label.textContent = name;
    button.append(createIcon("folder-line"), label);
    container.append(button);
  }
}

async function refreshFolders() {
  const request = ++state.folderRequest;
  try {
    const current = await api("folders");
    if (request !== state.folderRequest) return;
    if (current.length === customFolders.size && current.every(({ id, label }) => customFolders.get(id) === label)) return;
    customFolders.clear();
    for (const folder of current) customFolders.set(folder.id, folder.label);
    renderFolderNav();
    if (!(state.folder in folders) && !customFolders.has(state.folder)) {
      selectFolder("inbox");
    } else {
      updateFolderHeading();
      if (state.current) renderMoveOptions(state.current);
      for (const row of byId("messages").querySelectorAll("[data-mail-folder]")) {
        row.querySelector(".row-folder").textContent = mailFolderName(row.dataset.mailFolder);
      }
    }
  } catch (error) {
    if (request === state.folderRequest) toast(error.message);
  }
}

function renderMoveOptions(item) {
  const control = byId("move-folder");
  control.replaceChildren(new Option("移动到…", ""));
  control.hidden = Boolean(item.delivery) || item.folder === "drafts";
  for (const [id, name] of [["inbox", folders.inbox], ["archive", folders.archive], ...customFolders]) {
    if (id !== item.folder) control.add(new Option(name, id));
  }
}

function mailFolderName(folder) {
  return folders[folder] || customFolders.get(folder) || "文件夹";
}

function rowFor(item) {
  const row = document.createElement("button");
  row.className = item.read ? "row" : "row unread";
  row.dataset.id = item.id;
  row.toggleAttribute("data-delivery", Boolean(item.delivery));
  row.toggleAttribute("data-selectable", !item.delivery && (item.folder !== "drafts" || state.folder === "drafts"));
  row.setAttribute("aria-current", String(state.current?.id === item.id));
  const display = item.folder === "drafts" || item.folder === "sent" || item.delivery
    ? (item.to || item.cc || item.bcc) : item.from;
  const values = {
    name: display || (item.folder === "drafts" ? "未填写收件人" : "退信"),
    time: shortDate.format(item.date * 1000),
    subject: item.subject || "（无主题）",
    preview: item.delivery ? outboxStatusLabel(item.delivery) : item.body,
  };
  if (canUndoOutbox(item.delivery)) row.dataset.undoExpires = item.delivery.undoExpires;
  if (state.folder === "all") {
    row.dataset.mailFolder = item.folder;
    values.folder = mailFolderName(item.folder);
  }
  if (item.snoozeUntil) values.snooze = "移回时间 " + fullDate.format(item.snoozeUntil * 1000);
  for (const [key, value] of Object.entries(values)) {
    const element = document.createElement("span");
    element.className = "row-" + key;
    if (key === "name" && row.hasAttribute("data-selectable")) {
      const icon = createIcon("checkbox-blank-line");
      icon.classList.add("batch-icon");
      element.append(icon);
    }
    if (key === "subject" && item.star) {
      const icon = createIcon("star-fill", "已加星");
      icon.classList.add("row-star");
      element.append(icon);
    }
    element.append(value ?? "");
    row.append(element);
  }
  row.addEventListener("click", event => {
    if (state.batchMode) {
      if (!state.batchPending && row.hasAttribute("data-selectable")) toggleBatchRow(item.id);
      return;
    }
    void openLetter(item.id, event.detail === 0);
  });
  return row;
}

async function load(append = false, quiet = false) {
  const request = ++state.request;
  state.loading = true;
  const messageList = byId("messages");
  const initialFocus = document.activeElement.closest("#messages .row")?.dataset.id;
  const text = byId("list-status");
  const more = byId("more");
  if (!append && !quiet) {
    state.pages = 0;
    messageList.replaceChildren();
  }
  more.hidden = true;
  if (!quiet) text.textContent = "正在读取邮件…";
  const filters = filterParams(state.filterValues);
  const query = new URLSearchParams({ folder: state.folder, q: state.query, ...filters });
  if (state.label) query.set("label", state.label);
  const endpoint = state.folder === "outbox" ? "outbox?" : "letters?";
  const pages = quiet ? Math.max(1, state.pages) : 1;
  if (append) query.set("before", state.next);
  try {
    const letters = [];
    let next, loaded = 0;
    do {
      const result = await api(endpoint + query);
      if (request !== state.request) return;
      for (const item of result.letters) {
        trackOutboxUndo(item);
        letters.push(item);
      }
      next = result.next;
      query.set("before", next);
      ++loaded;
    } while (next && loaded < pages);
    const previousSelection = messageList.querySelector('[aria-current="true"]');
    const focusedID = document.activeElement.closest("#messages .row")?.dataset.id ||
      (document.activeElement === document.body ? initialFocus : undefined);
    const rows = letters.map(rowFor);
    if (append) messageList.append(...rows);
    else messageList.replaceChildren(...rows);
    if (!append && state.current?.delivery) {
      const active = letters.find(item => item.id === state.current.id);
      if (active) {
        state.current.delivery = active.delivery;
        displayOutboxStatus(state.current);
        void refreshOutboxRecipients(active.id);
      } else {
        clearReading();
      }
    }
    if (!append && state.current && !state.current.delivery) {
      const current = state.current;
      const active = letters.find(item => item.id === current.id);
      if (active && active.snoozeUntil !== current.snoozeUntil) {
        renderLetter({ ...current, folder: active.folder, snoozeUntil: active.snoozeUntil });
      } else if (!active && quiet && previousSelection &&
          (state.folder === "snoozed" || (state.folder === "inbox" && !state.query && !Object.keys(filters).length))) {
        clearReading();
      }
    }
    state.pages = append ? state.pages + loaded : loaded;
    state.next = next;
    more.hidden = !state.next;
    const emptyMessage = state.label ? "此标签暂无邮件。" : state.folder === "snoozed" ? "暂无稍后处理的邮件。" : "此文件夹暂无邮件。";
    const searching = state.savedSearch || state.query || Object.keys(filters).length;
    text.textContent = messageList.children.length ? "" : searching ? "没有找到匹配的邮件。" : emptyMessage;
    renderBatchSelection();
    refreshOutboxUndoExpiry();
    if (!append && focusedID) focusMailList(focusedID);
  } catch (error) {
    if (request !== state.request) return;
    if (!quiet) text.textContent = error.message;
    more.hidden = quiet ? !state.next : !append;
  } finally {
    if (request === state.request) state.loading = false;
  }
}

function filterParams(values) {
  const { start, end, ...fields } = values;
  const dates = {};
  if (start) dates.since = new Date(start + "T00:00:00").getTime() / 1000;
  if (end) {
    const through = new Date(end + "T00:00:00");
    dates.until = through.setDate(through.getDate() + 1) / 1000;
  }
  if (Object.values(dates).some(value => !Number.isFinite(value)) || dates.since >= dates.until) return null;
  return { ...fields, ...dates };
}

function updateFilterButton() {
  const count = Object.keys(state.filterValues).length;
  byId("open-filters").textContent = count ? "筛选（" + count + "）" : "筛选";
}

function applySearch() {
  state.query = byId("search").value;
  updateFilterButton();
  clearReading();
  setBatchMode(false);
  void load();
}

function renderBatchOptions() {
  const control = byId("batch-action");
  const previous = control.value;
  const options = [["export", "导出所选 ZIP"]];
  if (state.folder !== "drafts") options.push(
    ["read", "标为已读"], ["unread", "标为未读"], ["star", "加星标"], ["unstar", "取消星标"],
    ["folder:inbox", state.folder === "junk" ? "非垃圾邮件，移回收件箱" : "移到收件箱"], ["folder:archive", "归档"],
    ["folder:junk", "标记为垃圾邮件"], ["folder:trash", "移到废纸篓"],
  );
  if (["trash", "archive"].includes(state.folder)) options.push(["folder:restore", "恢复邮件"]);
  for (const [id, name] of customFolders) {
    if (state.folder !== "drafts" && id !== state.folder) options.push(["folder:" + id, "移到「" + name + "」"]);
  }
  if (state.folder !== "drafts") {
    for (const { id, name } of mailLabels.values()) {
      options.push(["label-add:" + id, "添加标签「" + name + "」"], ["label-remove:" + id, "移除标签「" + name + "」"]);
    }
  }
  control.replaceChildren(new Option("选择操作", ""), ...options.map(([id, name]) => new Option(name, id)));
  if (options.some(([id]) => id === previous)) control.value = previous;
  byId("apply-batch").disabled = state.batchPending || !state.selected.size || !control.value;
}

function setBatchMode(active) {
  ++state.batchRevision;
  state.batchMode = active;
  state.selected.clear();
  byId("batch-action").value = "";
  byId("select-mails").setAttribute("aria-pressed", String(active));
  byId("batch-tools").hidden = !active;
  renderBatchSelection();
}

function batchRowIDs() {
  return Array.from(byId("messages").querySelectorAll("[data-selectable]"), row => Number(row.dataset.id));
}

function toggleBatchRow(id) {
  if (!state.selected.delete(id)) {
    if (state.selected.size >= batchLimit) {
      toast("每次最多选择 100 封邮件");
      return;
    }
    state.selected.add(id);
  }
  renderBatchSelection();
}

function renderBatchSelection() {
  const allowed = new Set(batchRowIDs());
  for (const id of state.selected) {
    if (!allowed.has(id)) state.selected.delete(id);
  }
  for (const row of byId("messages").children) {
    if (state.batchMode && row.hasAttribute("data-selectable")) {
      const selected = state.selected.has(Number(row.dataset.id));
      row.setAttribute("aria-pressed", String(selected));
      row.querySelector(".batch-icon use").setAttribute("href", selected ? "/icons.svg#checkbox-fill" : "/icons.svg#checkbox-blank-line");
    } else {
      row.removeAttribute("aria-pressed");
    }
    row.disabled = state.batchMode && (state.batchPending || !row.hasAttribute("data-selectable"));
  }
  const count = state.selected.size;
  byId("batch-count").textContent = "已选 " + count + " 封";
  const all = byId("batch-all");
  all.disabled = !allowed.size || state.batchPending;
  all.textContent = count && count >= Math.min(allowed.size, batchLimit) ? "清空选择"
    : (allowed.size > batchLimit ? "选择前 100 封" : "全选已加载");
  byId("batch-action").disabled = state.batchPending;
  byId("apply-batch").disabled = state.batchPending || !count || !byId("batch-action").value;
}

async function applyBatch() {
  const action = byId("batch-action").value;
  if (state.batchPending || !action || !state.selected.size) return;
  const revision = state.batchRevision;
  const ids = [...state.selected];
  const changes = { read: { read: true }, unread: { read: false }, star: { star: true }, unstar: { star: false } };
  const [kind, target] = action.split(":");
  let change = changes[action];
  if (kind === "folder") change = { folder: target, spamFeedback: target === "junk" || (target === "inbox" && state.folder === "junk") };
  if (kind === "label-add" || kind === "label-remove") {
    change = { label: { id: Number(target), add: kind === "label-add" } };
  }
  state.batchPending = true;
  renderBatchSelection();
  try {
    if (action === "export") {
      await downloadMailArchive({ ids });
      if (revision === state.batchRevision) toast("已生成邮件 ZIP");
      return;
    }
    await api("letters", "PATCH", { ids, ...change });
    if (revision === state.batchRevision) {
      state.selected.clear();
      if (ids.includes(state.current?.id)) clearReading();
      toast("已更新 " + ids.length + " 封邮件");
      await load(false, true);
    }
  } catch (error) {
    if (revision === state.batchRevision) toast(error.message);
  } finally {
    state.batchPending = false;
    renderBatchSelection();
  }
}

function renderLetter(item) {
  renderLetterLabels(item);
  if (state.current?.id !== item.id || Boolean(state.current?.delivery) !== Boolean(item.delivery)) {
    state.htmlMode = Boolean(item.hasHTML && !item.hasText);
    byId("letter-actions").open = false;
  }
  state.current = item;
  byId("subject").textContent = item.subject || "（无主题）";
  byId("sender").textContent = item.from || "退信";
  byId("add-sender").hidden = !item.senderAddress || ["sent", "drafts", "junk"].includes(item.folder);
  byId("recipient").textContent = item.to;
  for (const field of ["cc", "bcc"]) {
    const value = item[field] || "";
    const cell = byId("letter-" + field);
    cell.textContent = value;
    cell.hidden = byId("letter-" + field + "-label").hidden = !value;
  }
  byId("letter-date-label").textContent = "时间";
  byId("date").textContent = fullDate.format(item.date * 1000);
  byId("body").textContent = item.hasHTML && !item.hasText ? "此邮件没有纯文本正文。" : item.body;
  renderBodyFormat(item);
  const base = item.delivery ? "outbox" : "letters";
  byId("download").href = "/api/" + base + "/" + item.id + "/raw";
  const delivery = byId("delivery-status");
  const importedDraft = item.folder === "drafts" && !item.draftVersion;
  delivery.hidden = !item.delivery && !importedDraft;
  byId("cancel-scheduled").hidden = true;
  if (item.delivery) {
    displayOutboxStatus(item);
  } else if (importedDraft) {
    delivery.textContent = "此草稿可在邮件客户端继续编辑。";
  }
  renderOutboxRecipients(item.delivery);
  const links = byId("attachment-links");
  links.replaceChildren();
  for (const file of item.attachments || []) {
    const row = document.createElement("li");
    const link = document.createElement("a");
    link.href = "/api/letters/" + item.id + "/attachments/" + file.part;
    link.textContent = file.name;
    link.download = file.name;
    row.append(link);
    links.append(row);
  }
  byId("received-files").hidden = !links.children.length;
  byId("star-label").textContent = item.star ? "取消星标" : "加星";
  byId("star").querySelector("use").setAttribute("href", item.star ? "/icons.svg#star-fill" : "/icons.svg#star-line");
  byId("toggle-read").textContent = item.read ? "标为未读" : "标为已读";
  byId("archive").textContent = item.folder === "archive" || item.folder === "trash" ? "恢复邮件" : "归档";
  byId("star").hidden = Boolean(item.delivery);
  byId("toggle-read").hidden = Boolean(item.delivery);
  byId("archive").hidden = Boolean(item.delivery) || item.folder === "drafts" || item.folder === "junk";
  const junk = byId("mark-junk");
  junk.textContent = item.folder === "junk" ? "非垃圾邮件，移回收件箱" : "标记为垃圾邮件";
  junk.hidden = Boolean(item.delivery) || ["drafts", "sent", "trash"].includes(item.folder);
  byId("trash").hidden = Boolean(item.delivery) || item.folder === "trash";
  byId("purge-trash").hidden = Boolean(item.delivery) || item.folder !== "trash";
  byId("reply").hidden = !item.replyAddress || ["sent", "drafts", "junk"].includes(item.folder);
  byId("forward").hidden = !canForward(item);
  renderSnooze(item);
  renderDeliveryTimeline(item.timeline);
  byId("edit-draft").hidden = !item.draftVersion;
  renderMoveOptions(item);
  byId("no-selection").hidden = true;
  byId("letter").hidden = false;
  byId("mailbox").classList.add("open");
  for (const row of byId("messages").children) {
    const active = Number(row.dataset.id) === item.id;
    row.setAttribute("aria-current", String(active));
    if (active && item.read) row.classList.remove("unread");
  }
}

function renderBodyFormat(item) {
  const html = Boolean(item.hasHTML && state.htmlMode);
  byId("body").hidden = html;
  byId("html-body").hidden = !html;
  const button = byId("toggle-format");
  button.hidden = !item.hasHTML;
  button.textContent = html ? "查看纯文本" : "查看 HTML 正文";
  const frame = byId("html-frame");
  if (html) {
    const source = "/api/letters/" + item.id + "/html";
    if (frame.getAttribute("src") !== source) frame.setAttribute("src", source);
  } else {
    frame.removeAttribute("src");
  }
}

async function openLetter(id, focusSubject = false) {
  const request = ++state.reading;
  try {
    const item = await api((state.folder === "outbox" ? "outbox/" : "letters/") + id);
    if (request !== state.reading) return;
    trackOutboxUndo(item);
    if (state.current?.id !== id && !byId("conversation-list").querySelector('[data-thread-id="' + id + '"]')) {
      clearConversation();
    }
    renderLetter(item);
    refreshOutboxUndoExpiry();
    if (focusSubject) byId("subject").focus();
    void loadConversation(item, true);
    if (!state.recoveryMode && activeMailboxPreferences.readMode === "auto" && !item.read && !item.delivery && item.folder !== "drafts") {
      await api("letters/" + id, "PATCH", { read: true });
      if (request === state.reading && state.current?.id === id) {
        renderLetter({ ...state.current, read: true });
      }
    }
  } catch (error) {
    if (request === state.reading) toast(error.message);
  }
}

function clearConversation() {
  ++state.conversationRequest;
  conversation.hidden = true;
  conversation.open = false;
  byId("conversation-list").replaceChildren();
}

async function loadConversation(item, opened = false) {
  if (item.delivery || ["drafts", "junk", "trash"].includes(item.folder)) {
    clearConversation();
    return;
  }
  const request = ++state.conversationRequest;
  try {
    const messages = await api("letters/" + item.id + "/thread");
    if (request !== state.conversationRequest || state.current?.id !== item.id) return;
    if (messages.length < 2) {
      clearConversation();
      return;
    }
    renderConversation(messages, item.id);
  } catch {
    if (opened && request === state.conversationRequest) clearConversation();
  }
}

function renderConversation(messages, selectedID) {
  const list = byId("conversation-list");
  const focusedID = list.contains(document.activeElement) ? document.activeElement.dataset.threadId : null;
  list.replaceChildren(...messages.map(item => {
    const row = document.createElement("li");
    const button = document.createElement("button");
    button.type = "button";
    button.dataset.threadId = item.id;
    if (item.id === selectedID) button.setAttribute("aria-current", "true");
    const values = {
      contact: item.folder === "sent" ? "发给 " + item.to : item.from,
      date: mailFolderName(item.folder) + " / " + fullDate.format(item.date * 1000),
      subject: item.subject || "（无主题）",
    };
    for (const [name, value] of Object.entries(values)) {
      const span = document.createElement("span");
      span.className = "conversation-" + name;
      span.textContent = value;
      button.append(span);
    }
    button.addEventListener("click", () => { if (state.current?.id !== item.id) void openLetter(item.id); });
    row.append(button);
    return row;
  }));
  byId("conversation-summary").textContent = "同一会话（" + messages.length + " 封）";
  conversation.hidden = false;
  if (focusedID) list.querySelector('[data-thread-id="' + focusedID + '"]')?.focus({ preventScroll: true });
}

async function updateLetter(change, button) {
  const id = state.current?.id;
  if (!id) return;
  button.disabled = true;
  try {
    await api("letters/" + id, "PATCH", change);
    if (state.current?.id === id) {
      if (change.folder || (state.folder === "starred" && change.star === false)) clearReading();
      else renderLetter({ ...state.current, ...change });
    }
    await load();
  } catch (error) {
    toast(error.message);
  } finally {
    button.disabled = false;
  }
}

async function compose(reply, { mode = reply ? "reply" : "compose", body = "", quote = false,
  source = reply, subject = "", files = [], draft = null, to = "" } = {}) {
  const request = ++composeRequest;
  const identitiesPromise = api("sender-identities").catch(() => null);
  let signatures, signatureError;
  try {
    signatures = await api("signatures");
  } catch (error) {
    signatureError = error.message;
  }
  const identities = await identitiesPromise;
  if (request !== composeRequest || byId("mailbox").hidden || editor.open || forwardDialog.open ||
      (source && state.current?.id !== source.id)) return false;
  editorForm.reset();
  setScheduleVisible(false);
  replySource = reply?.id || 0;
  selectedFiles.splice(0, selectedFiles.length, ...files);
  renderSelectedFiles();
  byId("send-error").textContent = "";
  if (reply) {
    editorForm.elements.to.value = reply.replyAddress;
    editorForm.elements.subject.value = /^re:/i.test(reply.subject) ? reply.subject : "Re: " + reply.subject;
  } else {
    editorForm.elements.to.value = draft?.to ?? to;
    editorForm.elements.cc.value = draft?.cc ?? "";
    editorForm.elements.bcc.value = draft?.bcc ?? "";
    editorForm.elements.subject.value = draft?.subject ?? subject;
  }
  editorForm.elements.body.value = draft?.body ?? body;
  prepareComposeOptions(draft, reply, identities);
  prepareComposeSignature(signatures, mode, quote);
  beginDraftTracking(draft);
  editor.showModal();
  editorForm.elements[reply ? "body" : "to"].focus();
  if (signatureError) toast("未能加载签名设置，可以继续写信。");
  return true;
}

async function resumeDraft() {
  const draft = state.current;
  if (!draft?.draftVersion) return;
  const button = byId("edit-draft");
  button.disabled = true;
  try {
    const files = await loadLetterAttachments(draft);
    if (state.current !== draft || editor.open) return;
    await compose(null, { mode: "draft", source: draft, draft, files });
  } catch (error) {
    toast(error.message);
  } finally {
    button.disabled = false;
  }
}

async function loadLetterAttachments(item, signal) {
  const attachments = item.attachments || [];
  if (attachments.length > attachmentCountLimit) throw new Error(attachmentLimitMessage());
  const files = [];
  let bytes = 0;
  for (const { part, name } of attachments) {
    const response = await fetch("/api/letters/" + item.id + "/attachments/" + part, { signal });
    if (!response.ok) throw new Error("无法读取附件「" + name + "」，请刷新后重试。");
    const blob = await response.blob();
    bytes += blob.size;
    if (bytes > attachmentSizeLimit) throw new Error(attachmentLimitMessage());
    files.push(new File([blob], name));
  }
  return files;
}

function composePayload() {
  if (!selectedFiles.length) {
    const { to, cc, bcc, subject, body, author, formatted } = editorForm.elements;
    const data = { to: to.value, cc: cc.value, bcc: bcc.value, subject: subject.value, body: body.value,
      author: author.value, formatted: formatted.checked };
    return replySource ? { ...data, replyToId: replySource } : data;
  }
  const form = new FormData(editorForm);
  if (replySource) form.set("replyToId", String(replySource));
  for (const file of selectedFiles) form.append("attachments", file);
  return form;
}

function renderSelectedFiles() {
  const list = byId("selected-files");
  list.replaceChildren();
  selectedFiles.forEach((file, index) => {
    const row = document.createElement("li");
    const name = document.createElement("span");
    name.textContent = file.name;
    const remove = document.createElement("button");
    remove.type = "button";
    remove.textContent = "移除";
    remove.setAttribute("aria-label", "移除 " + file.name);
    remove.addEventListener("click", () => {
      selectedFiles.splice(index, 1);
      renderSelectedFiles();
      markDraftEdited();
      byId("attachments").focus();
    });
    row.append(name, remove);
    list.append(row);
  });
}

byId("attachments").addEventListener("change", (event) => {
  const picker = event.currentTarget;
  const files = [...selectedFiles, ...picker.files];
  picker.value = "";
  if (files.length > attachmentCountLimit || files.reduce((size, file) => size + file.size, 0) > attachmentSizeLimit) {
    byId("send-error").textContent = attachmentLimitMessage();
    return;
  }
  selectedFiles.splice(0, selectedFiles.length, ...files);
  byId("send-error").textContent = "";
  renderSelectedFiles();
  markDraftEdited();
});

async function submit(form, action, errorId) {
  const button = form.querySelector('button[type="submit"]');
  button.disabled = true;
  if (form === folderForm) {
    byId("delete-folder").disabled = true;
    byId("close-folder").disabled = true;
  }
  byId(errorId).textContent = "";
  try {
    await action(Object.fromEntries(new FormData(form)));
  } catch (error) {
    byId(errorId).textContent = error.message;
  } finally {
    button.disabled = false;
    if (form === folderForm) {
      byId("delete-folder").disabled = false;
      byId("close-folder").disabled = false;
    }
  }
}

byId("login-form").addEventListener("submit", (event) => {
  event.preventDefault();
  const form = event.currentTarget;
  void submit(form, async data => {
    try {
      openSession(await api("login", "POST", data));
      byId("login-mfa-field").hidden = true;
    } catch (error) {
      if (error.status === 428) {
        byId("login-mfa-field").hidden = false;
        form.elements.code.value = "";
        form.elements.code.focus();
      }
      throw error;
    }
  }, "login-error");
});
editorForm.addEventListener("submit", (event) => {
  event.preventDefault();
  void finishCompose("send");
});
byId("logout").addEventListener("click", async () => {
  try {
    await api("logout", "POST");
    showLogin();
  } catch (error) {
    toast(error.message);
  }
});
byId("folder-nav").addEventListener("click", (event) => {
  const button = event.target.closest("[data-folder]");
  if (button) selectFolder(button.dataset.folder);
});
byId("search-form").addEventListener("submit", (event) => {
  event.preventDefault();
  applySearch();
});
byId("search").addEventListener("input", () => {
  if (!byId("search").value && state.query) applySearch();
});
byId("open-filters").addEventListener("click", () => {
  filterForm.reset();
  for (const [name, value] of Object.entries(state.filterValues)) filterForm.elements[name].value = value;
  byId("filter-error").textContent = "";
  filterDialog.showModal();
});
byId("close-filters").addEventListener("click", () => filterDialog.close());
byId("clear-filters").addEventListener("click", () => {
  state.filterValues = {};
  filterDialog.close();
  applySearch();
});
filterForm.addEventListener("input", () => { byId("filter-error").textContent = ""; });
filterForm.addEventListener("submit", event => {
  event.preventDefault();
  const values = Object.fromEntries(Array.from(new FormData(filterForm), ([name, value]) => [name, value.trim()])
    .filter(([, value]) => value));
  if (!filterParams(values)) {
    byId("filter-error").textContent = "结束日期不能早于开始日期";
    return;
  }
  state.filterValues = values;
  filterDialog.close();
  applySearch();
});
byId("select-mails").addEventListener("click", () => {
  if (!state.batchMode) clearReading();
  setBatchMode(!state.batchMode);
});
byId("batch-all").addEventListener("click", () => {
  const ids = batchRowIDs().slice(0, batchLimit);
  if (state.selected.size >= ids.length) state.selected.clear();
  else state.selected = new Set(ids);
  renderBatchSelection();
});
byId("batch-action").addEventListener("change", renderBatchSelection);
byId("batch-form").addEventListener("submit", event => {
  event.preventDefault();
  void applyBatch();
});
byId("refresh").addEventListener("click", async () => {
  await Promise.all([refreshFolders(), refreshMailLabels(), refreshSavedSearches()]);
  if (state.current) void refreshLetterLabels(state.current);
  const outbox = state.folder === "outbox";
  const current = outbox ? state.current : null;
  await load();
  if (current && state.folder === "outbox" && state.current?.id === current.id) {
    if (Array.from(byId("messages").children).some((row) => Number(row.dataset.id) === current.id)) {
      await openLetter(current.id);
    } else {
      clearReading();
    }
  }
});
byId("move-folder").addEventListener("change", (event) => {
  const control = event.currentTarget;
  const folder = control.value;
  control.value = "";
  if (folder) void updateLetter({ folder }, control);
});

function openFolderDialog(id = "") {
  folderForm.dataset.id = id;
  folderForm.elements.label.value = customFolders.get(id) || "";
  byId("folder-dialog-title").textContent = id ? "管理文件夹" : "新建文件夹";
  byId("delete-folder").hidden = !id;
  byId("folder-error").textContent = "";
  folderDialog.showModal();
  folderForm.elements.label.focus();
}

byId("create-folder").addEventListener("click", () => openFolderDialog());
byId("manage-folder").addEventListener("click", () => openFolderDialog(state.folder));
byId("close-folder").addEventListener("click", () => folderDialog.close());
folderDialog.addEventListener("cancel", (event) => {
  if (byId("close-folder").disabled) event.preventDefault();
});
folderForm.addEventListener("submit", (event) => {
  event.preventDefault();
  const id = folderForm.dataset.id;
  void submit(folderForm, async (data) => {
    const result = await api(id ? "folders/" + id : "folders", id ? "PUT" : "POST", data);
    folderDialog.close();
    await refreshFolders();
    if (customFolders.has(result.id)) selectFolder(result.id);
  }, "folder-error");
});
byId("delete-folder").addEventListener("click", async (event) => {
  const button = event.currentTarget;
  const id = folderForm.dataset.id;
  const save = folderForm.querySelector('button[type="submit"]');
  button.disabled = true;
  save.disabled = true;
  byId("close-folder").disabled = true;
  byId("folder-error").textContent = "";
  try {
    await api("folders/" + id, "DELETE");
    folderDialog.close();
    await refreshFolders();
    toast("文件夹已删除");
  } catch (error) {
    byId("folder-error").textContent = error.message;
  } finally {
    button.disabled = false;
    save.disabled = false;
    byId("close-folder").disabled = false;
  }
});

byId("show-addresses").addEventListener("click", async () => {
  const request = ++addressRequest;
  const content = byId("addresses-data");
  const status = byId("addresses-status");
  content.hidden = true;
  status.textContent = "正在读取收信地址…";
  addressesDialog.showModal();
  try {
    const { primary, aliases } = await api("addresses");
    if (request !== addressRequest || !addressesDialog.open) return;
    byId("primary-address").textContent = primary;
    byId("alias-list").replaceChildren(...aliases.map(alias => {
      const entry = document.createElement("li");
      entry.textContent = alias;
      return entry;
    }));
    byId("no-aliases").hidden = aliases.length !== 0;
    status.textContent = "";
    content.hidden = false;
  } catch (error) {
    if (request === addressRequest && addressesDialog.open) status.textContent = error.message;
  }
});
byId("close-addresses").addEventListener("click", () => addressesDialog.close());

function setupMailRules() {
  const overview = byId("rules-overview");
  const form = byId("rule-form");
  const controls = form.elements;
  const create = byId("create-rule");
  const remove = byId("delete-rule");
  const close = byId("close-rules");
  const cancel = byId("cancel-rule");
  const save = form.querySelector('[type="submit"]');
  const fields = { sender: "发件地址等于", domain: "发件域名等于", recipient: "收件地址等于", subject: "主题包含" };
  const actions = { star: "加星标", read: "标为已读" };
  const examples = { sender: "billing@example.com", domain: "example.com", recipient: "orders@example.com", subject: "账单" };
  const envelopeHelp = {
    sender: "匹配 SMTP 信封里的发件地址。",
    recipient: "匹配实际投递的收件地址，可以填写收信别名。",
    domain: "匹配 SMTP 信封的完整发件域名。",
  };
  let destinations = new Map();
  let ruleLabels = new Map();
  let editing = null;
  let generation = 0;

  function updateFields() {
    controls.value.placeholder = examples[controls.field.value];
    const help = byId("rule-match-help");
    help.textContent = envelopeHelp[controls.field.value] || "";
    help.hidden = !help.textContent;
    byId("rule-destination").hidden = controls.action.value !== "move";
    const needsLabel = controls.action.value === "label";
    byId("rule-label-target").hidden = !needsLabel;
    controls.labelID.disabled = !needsLabel;
  }

  function edit(rule = null) {
    editing = rule;
    form.reset();
    byId("rule-error").textContent = "";
    byId("rule-form-title").textContent = rule ? "编辑规则" : "新建规则";
    controls.destination.replaceChildren(...Array.from(destinations, ([id, label]) => new Option(label, id)));
    controls.labelID.replaceChildren(new Option("选择标签", ""),
      ...Array.from(ruleLabels, ([id, label]) => new Option(label, id)));
    if (rule) {
      for (const key of ["order", "field", "value", "action"]) controls[key].value = rule[key];
      controls.enabled.checked = rule.enabled;
      if (rule.destination) controls.destination.value = rule.destination;
      if (rule.labelID) controls.labelID.value = rule.labelID;
    }
    remove.hidden = !rule;
    updateFields();
    overview.hidden = true;
    form.hidden = false;
    controls.field.focus();
  }

  function render(rules) {
    byId("rules-list").replaceChildren(...rules.map(rule => {
      const row = document.createElement("li");
      const order = document.createElement("span");
      order.className = "rule-order";
      order.textContent = rule.order;
      const summary = document.createElement("div");
      summary.className = "rule-summary";
      summary.textContent = fields[rule.field] + "「" + rule.value + "」";
      const action = document.createElement("span");
      action.className = "rule-help";
      const description = rule.action === "label" ? "添加标签「" + ruleLabels.get(rule.labelID) + "」"
        : actions[rule.action] || "移动到「" + destinations.get(rule.destination) + "」";
      action.textContent = rule.enabled ? description : description + "（已停用）";
      summary.append(action);
      const button = document.createElement("button");
      button.type = "button";
      button.className = "secondary";
      button.textContent = "编辑";
      button.setAttribute("aria-label", "编辑规则 " + rule.value);
      button.addEventListener("click", () => edit(rule));
      row.append(order, summary, button);
      return row;
    }));
    byId("rules-status").textContent = rules.length === 0 ? "还没有邮件规则。" : "";
  }

  async function loadRules() {
    const request = ++generation;
    editing = null;
    form.hidden = true;
    overview.hidden = false;
    create.disabled = true;
    byId("rules-list").replaceChildren();
    byId("rules-status").textContent = "正在读取规则…";
    try {
      const [rules, custom, labels] = await Promise.all([api("rules"), api("folders"), api("labels")]);
      if (request !== generation || !rulesDialog.open) return;
      destinations = new Map([
        ["archive", folders.archive], ["junk", folders.junk], ["inbox", folders.inbox],
        ...custom.map(({ id, label }) => [id, label]),
      ]);
      ruleLabels = new Map(labels.map(({ id, name }) => [id, name]));
      byId("rule-label-help").hidden = ruleLabels.size > 0;
      render(rules);
      create.disabled = false;
    } catch (error) {
      if (request === generation && rulesDialog.open) byId("rules-status").textContent = error.message;
    }
  }

  async function changeRule(method, payload) {
    const request = generation;
    const path = editing ? "rules/" + editing.id : "rules";
    for (const button of [close, cancel, remove, save]) button.disabled = true;
    byId("rule-error").textContent = "";
    try {
      await api(path, method, payload);
      if (request === generation && rulesDialog.open) await loadRules();
    } catch (error) {
      if (request === generation && rulesDialog.open) byId("rule-error").textContent = error.message;
    } finally {
      for (const button of [close, cancel, remove, save]) button.disabled = false;
    }
  }

  byId("manage-rules").addEventListener("click", () => {
    rulesDialog.showModal();
    void loadRules();
  });
  close.addEventListener("click", () => rulesDialog.close());
  rulesDialog.addEventListener("cancel", event => { if (close.disabled) event.preventDefault(); });
  rulesDialog.addEventListener("close", () => { ++generation; });
  create.addEventListener("click", () => edit());
  cancel.addEventListener("click", () => {
    form.hidden = true;
    overview.hidden = false;
    create.focus();
  });
  controls.field.addEventListener("change", updateFields);
  controls.action.addEventListener("change", updateFields);
  form.addEventListener("submit", event => {
    event.preventDefault();
    if (save.disabled) return;
    const data = Object.fromEntries(new FormData(form));
    data.order = Number(data.order);
    if (data.action === "label") data.labelID = Number(data.labelID);
    data.enabled = controls.enabled.checked;
    void changeRule(editing ? "PUT" : "POST", data);
  });
  remove.addEventListener("click", () => {
    if (confirm("删除这条邮件规则？")) void changeRule("DELETE");
  });
}

setupMailRules();
document.addEventListener("click", event => {
  if (event.target.closest("dialog")) return;
  for (const disclosure of document.querySelectorAll(".disclosure[open]")) {
    if (!disclosure.contains(event.target)) disclosure.open = false;
  }
});
document.addEventListener("keydown", event => {
  if (event.key !== "Escape" || event.defaultPrevented) return;
  const disclosure = event.target.closest(".disclosure[open]");
  if (!disclosure || event.target.matches("select")) return;
  event.preventDefault();
  disclosure.open = false;
  disclosure.querySelector("summary").focus();
});
byId("more").addEventListener("click", event => void showMoreLetters(event.detail === 0));
byId("back").addEventListener("click", returnToMailList);
byId("compose").addEventListener("click", () => void compose());
byId("edit-draft").addEventListener("click", resumeDraft);
byId("reply").addEventListener("click", () => void compose(state.current));
byId("star").addEventListener("click", (event) => updateLetter({ star: !state.current.star }, event.currentTarget));
byId("toggle-read").addEventListener("click", (event) => updateLetter({ read: !state.current.read }, event.currentTarget));
byId("toggle-format").addEventListener("click", () => {
  if (!state.current?.hasHTML) return;
  state.htmlMode = !state.htmlMode;
  renderBodyFormat(state.current);
});
byId("archive").addEventListener("click", (event) => {
  const folder = state.current.folder;
  void updateLetter({ folder: folder === "archive" || folder === "trash" ? "restore" : "archive" }, event.currentTarget);
});
byId("trash").addEventListener("click", (event) => updateLetter({ folder: "trash" }, event.currentTarget));
byId("purge-trash").addEventListener("click", async event => {
  const item = state.current;
  if (item?.folder !== "trash" || item.delivery || !confirm("永久删除这封邮件及其附件？此操作无法撤销。")) return;
  const button = event.currentTarget;
  button.disabled = true;
  try {
    await api("letters/" + item.id, "DELETE");
    if (state.current?.id === item.id && !state.current.delivery) clearReading();
    await load();
  } catch (error) {
    toast(error.message);
  } finally {
    button.disabled = false;
  }
});
byId("mark-junk").addEventListener("click", (event) => {
  void updateLetter({ folder: state.current.folder === "junk" ? "inbox" : "junk", spamFeedback: true }, event.currentTarget);
});

api("session").then(session => openSession(session, new URLSearchParams(location.search).get("view") === "mail")).catch(showLogin);
