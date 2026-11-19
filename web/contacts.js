"use strict";

{
  const dialog = byId("contacts-dialog");
  const overview = byId("contacts-overview");
  const list = byId("contact-list");
  const search = byId("contact-search");
  const status = byId("contacts-status");
  const more = byId("more-contacts");
  const create = byId("new-contact");
  const form = byId("contact-form");
  const error = byId("contact-error");
  const remove = byId("delete-contact");
  const close = byId("close-contacts");
  const cancel = byId("cancel-contact");
  const save = form.querySelector('[type="submit"]');
  const recipient = byId("compose-recipient");
  const options = byId("recipient-options");
  let request = 0;
  let searchTimer;
  let cursor = 0;
  let editing;
  let suggestionRequest = 0;
  let suggestionTimer;
  let suggestions = [];
  let activeOption = -1;

  function contactText(item) {
    const block = document.createElement("span");
    block.className = "contact-summary";
    const name = document.createElement("strong");
    name.textContent = item.name || item.email;
    block.append(name);
    if (item.name) {
      const email = document.createElement("span");
      email.textContent = item.email;
      block.append(email);
    }
    return block;
  }

  function contactRow(item) {
    const row = document.createElement("li");
    const actions = document.createElement("div");
    actions.className = "contact-actions";
    const write = document.createElement("button");
    write.className = "secondary";
    write.textContent = "写信";
    write.setAttribute("aria-label", "写信给 " + (item.name || item.email));
    write.addEventListener("click", async () => {
      dialog.close();
      if (!(await compose(null, { to: item.email }))) return;
      editorForm.elements.subject.focus();
    });
    const edit = document.createElement("button");
    edit.className = "secondary";
    edit.textContent = "编辑";
    edit.setAttribute("aria-label", "编辑联系人 " + (item.name || item.email));
    edit.addEventListener("click", () => editContact(item));
    actions.append(write, edit);
    row.append(contactText(item), actions);
    return row;
  }

  async function loadContacts(append = false) {
    const current = ++request;
    const query = new URLSearchParams({ q: search.value.trim() });
    if (append) {
      query.set("before", cursor);
    } else {
      list.replaceChildren();
      cursor = 0;
    }
    more.hidden = true;
    status.textContent = "正在读取联系人…";
    try {
      const result = await api("contacts?" + query);
      if (request !== current || !dialog.open) return;
      list.append(...result.contacts.map(contactRow));
      cursor = result.next;
      more.hidden = !cursor;
      status.textContent = list.children.length ? "" : (query.get("q") ? "没有找到匹配的联系人。" : "通讯录暂无联系人。");
    } catch (err) {
      if (request === current && dialog.open) {
        status.textContent = err.message;
        more.hidden = !cursor;
      }
    }
  }

  function showOverview() {
    clearTimeout(searchTimer);
    form.hidden = true;
    overview.hidden = false;
    search.value = "";
    void loadContacts();
  }

  function editContact(item = {}) {
    ++request;
    clearTimeout(searchTimer);
    editing = item.id || 0;
    form.reset();
    form.elements.namedItem("name").value = Array.from(item.name || "").slice(0, 100).join("");
    form.elements.email.value = item.email || "";
    form.elements.note.value = item.note || "";
    byId("contact-form-title").textContent = editing ? "编辑联系人" : "新建联系人";
    remove.hidden = !editing;
    error.textContent = "";
    overview.hidden = true;
    form.hidden = false;
    form.elements.namedItem(item.email ? "name" : "email").focus();
  }

  async function writeContact(method, data) {
    const current = request;
    for (const button of [save, remove, close, cancel]) button.disabled = true;
    error.textContent = "";
    try {
      await api(editing ? "contacts/" + editing : "contacts", method, data);
      if (request === current && dialog.open) showOverview();
    } catch (err) {
      if (request === current && dialog.open) error.textContent = err.message;
    } finally {
      for (const button of [save, remove, close, cancel]) button.disabled = false;
    }
  }

  byId("show-contacts").addEventListener("click", () => {
    dialog.showModal();
    showOverview();
  });
  byId("add-sender").addEventListener("click", async () => {
    const message = state.current;
    if (!message?.senderAddress) return;
    const current = ++request;
    const initial = { name: message.senderName, email: message.senderAddress };
    overview.hidden = false;
    form.hidden = true;
    list.replaceChildren();
    more.hidden = true;
    search.value = "";
    status.textContent = "正在查找发件人…";
    create.disabled = true;
    search.disabled = true;
    dialog.showModal();
    try {
      const { contacts } = await api("contacts?" + new URLSearchParams({ email: initial.email, limit: 1 }));
      if (request === current && dialog.open) editContact(contacts[0] || initial);
    } catch (err) {
      if (request === current && dialog.open) {
        status.textContent = err.message;
        search.focus();
      }
    } finally {
      create.disabled = false;
      search.disabled = false;
    }
  });
  close.addEventListener("click", () => dialog.close());
  dialog.addEventListener("cancel", event => { if (close.disabled) event.preventDefault(); });
  dialog.addEventListener("close", () => {
    if (!dialog.open) {
      ++request;
      clearTimeout(searchTimer);
      list.replaceChildren();
      form.reset();
    }
  });
  search.addEventListener("input", () => {
    ++request;
    more.hidden = true;
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => { if (dialog.open) void loadContacts(); }, 160);
  });
  more.addEventListener("click", () => loadContacts(true));
  create.addEventListener("click", () => editContact());
  cancel.addEventListener("click", showOverview);
  form.addEventListener("submit", event => {
    event.preventDefault();
    if (save.disabled) return;
    void writeContact(editing ? "PUT" : "POST", Object.fromEntries(new FormData(form)));
  });
  remove.addEventListener("click", () => {
    if (confirm("从通讯录中删除此联系人？")) void writeContact("DELETE");
  });

  function closeSuggestions() {
    ++suggestionRequest;
    clearTimeout(suggestionTimer);
    options.hidden = true;
    options.replaceChildren();
    suggestions = [];
    activeOption = -1;
    recipient.setAttribute("aria-expanded", "false");
    recipient.removeAttribute("aria-activedescendant");
  }

  function chooseRecipient(item) {
    const entries = recipient.value.split(/[,;，；]/);
    entries[entries.length - 1] = item.email;
    recipient.value = entries.map(entry => entry.trim()).filter(Boolean).join(", ");
    markDraftEdited();
    closeSuggestions();
    recipient.focus();
  }

  function renderSuggestions(items) {
    suggestions = items;
    if (!suggestions.length) return;
    options.replaceChildren(...items.map((item, index) => {
      const option = document.createElement("div");
      option.id = "recipient-option-" + index;
      option.setAttribute("role", "option");
      option.setAttribute("aria-selected", "false");
      option.append(contactText(item));
      option.addEventListener("click", () => chooseRecipient(item));
      return option;
    }));
    options.hidden = false;
    recipient.setAttribute("aria-expanded", "true");
  }

  recipient.addEventListener("input", () => {
    closeSuggestions();
    const entries = recipient.value.split(/[,;，；]/).map(entry => entry.trim());
    const query = entries.pop();
    if (!query) return;
    const current = suggestionRequest;
    suggestionTimer = setTimeout(async () => {
      try {
        const { contacts } = await api("contacts?" + new URLSearchParams({ q: query, limit: 8 }));
        if (current === suggestionRequest && editor.open && document.activeElement === recipient) {
          renderSuggestions(contacts.filter(item => item.email !== query && !entries.includes(item.email)));
        }
      } catch {
        // Manual address entry stays available if suggestions cannot load.
      }
    }, 160);
  });
  recipient.addEventListener("blur", closeSuggestions);
  options.addEventListener("pointerdown", event => event.preventDefault());
  editor.addEventListener("close", closeSuggestions);
  recipient.addEventListener("keydown", event => {
    if (options.hidden) return;
    if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      closeSuggestions();
    } else if (event.key === "Enter" && activeOption >= 0) {
      event.preventDefault();
      chooseRecipient(suggestions[activeOption]);
    } else if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      const direction = event.key === "ArrowDown" ? 1 : -1;
      activeOption = activeOption < 0 && direction < 0
        ? suggestions.length - 1
        : (activeOption + direction + suggestions.length) % suggestions.length;
      for (const [index, option] of Array.from(options.children).entries()) {
        option.setAttribute("aria-selected", String(index === activeOption));
      }
      const active = options.children[activeOption];
      recipient.setAttribute("aria-activedescendant", active.id);
      active.scrollIntoView({ block: "nearest" });
    }
  });
}
