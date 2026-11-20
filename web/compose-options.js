"use strict";

function prepareComposeOptions(draft, reply, identities) {
  const { author, formatted } = editorForm.elements;
  for (const button of editorForm.querySelectorAll("[data-recipient-field]")) {
    const input = editorForm.elements[button.dataset.recipientField];
    showComposeControl(button, Boolean(input.value.trim()));
  }
  showComposeControl(byId("toggle-compose-format"), false);
  byId("compose-more").open = false;
  editor.removeAttribute("data-expanded");
  byId("expand-compose").setAttribute("aria-pressed", "false");
  byId("expand-compose").textContent = "展开";
  const { primary, aliases } = identities || { primary: byId("account").textContent, aliases: [] };
  author.replaceChildren(new Option(primary, ""), ...aliases.map(alias => new Option(alias, alias)));
  const selected = draft?.from || aliases.find(alias => alias === reply?.to) || "";
  if (selected && selected !== primary && !aliases.includes(selected) && draft) {
    author.add(new Option(selected + "（权限已变更）", selected));
  }
  author.value = selected === primary ? "" : selected;
  byId("compose-author-row").hidden = author.options.length < 2;
  formatted.checked = Boolean(draft?.formatted);
  byId("compose-format-tools").hidden = !formatted.checked;
  byId("compose-preview").hidden = true;
  byId("compose-preview").removeAttribute("srcdoc");
}

function showComposeControl(button, visible) {
  byId(button.getAttribute("aria-controls")).hidden = !visible;
  button.setAttribute("aria-expanded", String(visible));
}

{
  const more = byId("compose-more");
  const expand = byId("expand-compose");
  const formatToggle = byId("toggle-compose-format");
  for (const button of editorForm.querySelectorAll("[data-recipient-field]")) {
    button.addEventListener("click", () => {
      const input = editorForm.elements[button.dataset.recipientField];
      const visible = button.getAttribute("aria-expanded") !== "true" || Boolean(input.value.trim());
      showComposeControl(button, visible);
      if (visible) input.focus();
    });
  }
  expand.addEventListener("click", () => {
    const expanded = editor.toggleAttribute("data-expanded");
    expand.setAttribute("aria-pressed", String(expanded));
    expand.textContent = expanded ? "还原" : "展开";
  });
  editor.addEventListener("click", event => {
    if (!more.contains(event.target)) more.open = false;
  });
  byId("templates-dialog").addEventListener("close", () => {
    if (!more.open) return;
    if (more.contains(document.activeElement)) more.querySelector("summary").focus();
    more.open = false;
  });
  byId("add-compose-attachment").addEventListener("click", () => byId("attachments").click());
  formatToggle.addEventListener("click", () => {
    showComposeControl(formatToggle, byId("compose-format-panel").hidden);
  });

  const { formatted, body } = editorForm.elements;
  const tools = byId("compose-format-tools");
  const preview = byId("compose-preview");
  formatted.addEventListener("input", () => {
    tools.hidden = !formatted.checked;
    preview.hidden = true;
  });
  body.addEventListener("input", () => { preview.hidden = true; });
  tools.addEventListener("click", event => {
    const format = event.target.closest("[data-format]")?.dataset.format;
    if (!format) return;
    const start = body.selectionStart;
    const end = body.selectionEnd;
    const selection = body.value.slice(start, end);
    const replacement = {
      bold: "**" + (selection || "加粗文字") + "**",
      link: "[" + (selection || "链接名称") + "](https://example.com)",
      list: (selection || "列表项").split("\n").map(line => "- " + line).join("\n"),
      quote: (selection || "引用内容").split("\n").map(line => "> " + line).join("\n")
    }[format];
    const block = format === "list" || format === "quote";
    const prefix = block && start > 0 && body.value[start - 1] !== "\n" ? "\n" : "";
    body.setRangeText(prefix + replacement, start, end, "end");
    body.focus();
    if (format === "link") body.setSelectionRange(body.selectionStart - "https://example.com)".length, body.selectionStart - 1);
    body.dispatchEvent(new Event("input", { bubbles: true }));
  });
  byId("preview-compose").addEventListener("click", async event => {
    const button = event.currentTarget;
    const source = body.value;
    button.disabled = true;
    try {
      const { html } = await api("compose-preview", "POST", { body: source });
      if (!editor.open || !formatted.checked || body.value !== source) return;
      preview.srcdoc = '<!doctype html><meta charset="utf-8"><style>body{font:15px system-ui;margin:12px;overflow-wrap:anywhere}</style>' + html;
      preview.hidden = false;
    } catch (error) {
      toast(error.message);
    } finally {
      button.disabled = false;
    }
  });
}
