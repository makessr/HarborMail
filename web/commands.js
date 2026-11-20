"use strict";

const commandsDialog = document.getElementById("commands-dialog");
const commandQuery = document.getElementById("command-query");
const commandResults = document.getElementById("command-results");
let matchingCommands = [];
let activeCommand = 0;

function focusMailList(id) {
  const rows = Array.from(byId("messages").children);
  const row = rows.find(item => item.dataset.id === String(id)) || rows[0];
  (row || byId("folder-title")).focus();
}

function returnToMailList() {
  const id = state.current?.id;
  clearReading();
  focusMailList(id);
}

async function showMoreLetters(fromKeyboard) {
  const previousLast = byId("messages").lastElementChild;
  const loading = load(true);
  const request = state.request;
  await loading;
  if (fromKeyboard && request === state.request && [document.body, byId("more")].includes(document.activeElement)) {
    (previousLast?.nextElementSibling || previousLast || byId("folder-title")).focus();
  }
}

async function navigateFromCommands(loadDestination) {
  const loading = loadDestination();
  if (!loading) return; // Recovering a saved search opens its editor.
  const heading = byId("folder-title");
  heading.focus();
  await loading;
  if (document.activeElement === heading && !document.querySelector("dialog[open]")) focusMailList();
}

function buildCommands(query) {
  const composeAction = { name: "写邮件", kind: "操作", icon: "draft-line", run: () => byId("compose").click() };
  const readingActions = [];
  if (state.current) {
    const { id, delivery } = state.current;
    for (const [control, icon] of [
      ["reply", "reply-line"], ["forward", "share-forward-line"], ["edit-draft", "draft-line"],
      ["star", "star-line"], ["toggle-read", "mail-line"], ["archive", "archive-line"], ["edit-snooze", "time-line"],
    ]) {
      const button = byId(control);
      if (button.hidden || button.disabled) continue;
      const name = button.textContent.trim();
      readingActions.push({ name, kind: "当前邮件", icon, run: () => {
        if (state.current?.id !== id || Boolean(state.current.delivery) !== Boolean(delivery) ||
            button.hidden || button.disabled || button.textContent.trim() !== name) {
          toast("邮件状态已变化，请重新打开快捷操作");
          return;
        }
        button.click();
      } });
    }
  }
  const destinations = [
    ...Object.entries(folders).map(([id, name]) => ({ name, kind: "文件夹", icon: "folder-line", run: () => selectFolder(id) })),
    ...Array.from(customFolders, ([id, name]) => ({ name, kind: "文件夹", icon: "folder-line", run: () => {
      if (!customFolders.has(id)) { toast("该文件夹已不可用"); return; }
      return selectFolder(id);
    } })),
    ...Array.from(mailLabels.values(), ({ id, name }) => ({ name, kind: "标签", icon: "price-tag-3-line", run: () => {
      if (!mailLabels.has(id)) { toast("该标签已不可用"); return; }
      return selectFolder("all", id);
    } })),
    ...Array.from(savedSearches.values(), ({ id, name }) => ({ name, kind: "保存的搜索", icon: "search-eye-line", run: () => {
      const item = savedSearches.get(id);
      if (!item) { toast("该搜索已不可用"); return; }
      return openSavedSearch(item);
    } })),
  ].map(action => ({ ...action, run: () => void navigateFromCommands(action.run) }));
  const utilities = [
    { name: "定位邮件列表", kind: "操作", icon: "inbox-line", run: returnToMailList },
    { name: "搜索当前视图", kind: "操作", icon: "search-line", run: () => {
      if (byId("reading-commands").getClientRects().length) clearReading();
      byId("search").focus();
    } },
    { name: "邮箱偏好", kind: "操作", icon: "settings-3-line", run: () => byId("show-preferences").click() },
  ];
  const all = [composeAction, ...readingActions, ...destinations, ...utilities];
  if (!query) return all;
  const terms = query.toLowerCase().split(/\s+/);
  const matches = all.filter(({ name, kind }) => {
    const text = (name + " " + kind).toLowerCase();
    return terms.every(term => text.includes(term));
  });
  matches.push({ name: "搜索邮件「" + query + "」", kind: "全部邮件", icon: "search-line",
    run: () => void navigateFromCommands(() => selectFolder("all", 0, null, query)) });
  return matches;
}

function chooseCommand(index, scroll = false) {
  commandResults.children[activeCommand]?.setAttribute("aria-selected", "false");
  activeCommand = index;
  const option = commandResults.children[index];
  option.setAttribute("aria-selected", "true");
  commandQuery.setAttribute("aria-activedescendant", option.id);
  if (scroll) option.scrollIntoView({ block: "nearest" });
}

function renderCommands() {
  matchingCommands = buildCommands(commandQuery.value.trim());
  activeCommand = 0;
  const options = matchingCommands.map(({ name, kind, icon }, index) => {
    const option = document.createElement("li");
    option.id = "command-option-" + index;
    option.setAttribute("role", "option");
    option.setAttribute("aria-selected", "false");
    option.dataset.index = index;
    const title = document.createElement("span");
    title.className = "command-name";
    title.textContent = name;
    title.title = name;
    const category = document.createElement("span");
    category.className = "command-category";
    category.textContent = kind;
    option.append(createIcon(icon), title, category);
    return option;
  });
  commandResults.replaceChildren(...options);
  commandResults.scrollTop = 0;
  chooseCommand(0);
}

function closeCommands() {
  if (commandsDialog.open) commandsDialog.close();
  commandQuery.value = "";
  commandQuery.removeAttribute("aria-activedescendant");
  commandResults.replaceChildren();
  matchingCommands = [];
}

function openCommands() {
  if (byId("mailbox").hidden || document.querySelector("dialog[open]")) return;
  renderCommands();
  commandsDialog.showModal();
  commandQuery.focus();
}

function runCommand(index) {
  const command = matchingCommands[index];
  if (!command) return;
  closeCommands();
  command.run();
}

function isKeyComposition(event) {
  return event.isComposing || event.keyCode === 229;
}

document.addEventListener("DOMContentLoaded", () => {
  byId("show-commands").addEventListener("click", openCommands);
  byId("reading-commands").addEventListener("click", openCommands);
  byId("close-commands").addEventListener("click", closeCommands);
  commandsDialog.addEventListener("cancel", event => {
    event.preventDefault();
    closeCommands();
  });
  commandQuery.addEventListener("input", renderCommands);
  commandQuery.addEventListener("keydown", event => {
    if (isKeyComposition(event) || event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
    if (event.key === "Enter") {
      event.preventDefault();
      if (!event.repeat) runCommand(activeCommand);
    } else if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      const count = matchingCommands.length;
      chooseCommand((activeCommand + (event.key === "ArrowDown" ? 1 : -1) + count) % count, true);
    }
  });
  commandResults.addEventListener("mousedown", event => event.preventDefault());
  commandResults.addEventListener("click", event => {
    const option = event.target.closest("[data-index]");
    if (option) runCommand(Number(option.dataset.index));
  });
  const list = byId("messages");
  list.addEventListener("keydown", event => {
    if (isKeyComposition(event) || event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
    const row = event.target.closest(".row");
    if (!row) return;
    let next;
    switch (event.key) {
      case "ArrowDown": next = row.nextElementSibling || (!byId("more").hidden && byId("more")); break;
      case "ArrowUp": next = row.previousElementSibling; break;
      case "Home": next = list.firstElementChild; break;
      case "End": next = list.lastElementChild; break;
      default: return;
    }
    event.preventDefault();
    if (next) next.focus();
  });
  byId("more").addEventListener("keydown", event => {
    if (event.key !== "ArrowUp" || event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
    event.preventDefault();
    list.lastElementChild?.focus();
  });
  document.addEventListener("keydown", event => {
    if (event.defaultPrevented || event.repeat || isKeyComposition(event) || byId("mailbox").hidden ||
        document.querySelector("dialog[open]") ||
        document.activeElement.closest("input, textarea, select, [contenteditable]")) return;
    if (!event.altKey && !event.shiftKey && event.ctrlKey !== event.metaKey && event.key.toLowerCase() === "k") {
      event.preventDefault();
      openCommands();
    } else if (event.key === "Escape" && state.current &&
      document.activeElement.closest("#letter") && !document.activeElement.closest(".disclosure[open]")) {
      event.preventDefault();
      returnToMailList();
    }
  });
});
