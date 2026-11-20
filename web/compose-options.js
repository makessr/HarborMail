"use strict";

function prepareComposeOptions(draft, reply, identities) {
  const { author, formatted } = editorForm.elements;
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

{
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
