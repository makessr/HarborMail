"use strict";

const templatesDialog = document.getElementById("templates-dialog");
const templatesForm = document.getElementById("templates-form");
let mailTemplates = null;
let templateRequest = 0;
let editedTemplate = -1;

function closeMailTemplates() {
  ++templateRequest;
  mailTemplates = null;
  if (templatesDialog.open) templatesDialog.close();
  templatesForm.reset();
  byId("template-choice").replaceChildren();
  byId("template-preview-subject").textContent = "";
  byId("template-preview-body").textContent = "";
  byId("templates-status").textContent = "";
  byId("templates-error").textContent = "";
}

function showTemplateView(view) {
  const { elements } = templatesForm;
  const picking = view === "picker";
  const editing = view === "edit";
  byId("templates-picker").hidden = !picking || !mailTemplates.templates.length;
  byId("template-pick-actions").hidden = !picking;
  byId("template-fields").hidden = !editing;
  elements.templateName.disabled = elements.templateSubject.disabled = elements.templateBody.disabled = !editing;
  byId("template-edit-actions").hidden = !editing;
  byId("template-delete-actions").hidden = view !== "delete";
  byId("templates-error").textContent = "";
}

function previewMailTemplate() {
  const item = mailTemplates.templates[byId("template-choice").selectedIndex];
  const subject = byId("template-preview-subject");
  subject.textContent = item?.subject || "";
  subject.hidden = !item?.subject;
  byId("template-preview-body").textContent = item?.body || "";
}

function pickMailTemplate(index = 0) {
  showTemplateView("picker");
  const items = mailTemplates.templates;
  const choice = byId("template-choice");
  choice.replaceChildren(...items.map(item => new Option(item.name)));
  choice.selectedIndex = items.length ? Math.min(index, items.length - 1) : -1;
  previewMailTemplate();
  byId("templates-title").textContent = "邮件模板";
  byId("edit-template").hidden = byId("insert-template").hidden = !items.length;
  byId("new-template").hidden = items.length >= 20;
  byId("templates-status").textContent = items.length ? "" : "还没有邮件模板。新建一份，下次写信时即可插入。";
  (items.length ? choice : byId("new-template")).focus();
}

function editMailTemplate(index) {
  editedTemplate = index;
  const item = mailTemplates.templates[index];
  showTemplateView("edit");
  byId("templates-title").textContent = item ? "编辑模板" : "新建模板";
  byId("templates-status").textContent = "";
  byId("delete-template").hidden = !item;
  const { elements } = templatesForm;
  elements.templateName.value = item?.name || "";
  elements.templateSubject.value = item?.subject || "";
  elements.templateBody.value = item?.body || "";
  elements.templateName.focus();
}

async function openMailTemplates() {
  const request = ++templateRequest;
  showTemplateView("loading");
  byId("templates-title").textContent = "邮件模板";
  byId("templates-status").textContent = "正在读取模板…";
  templatesDialog.showModal();
  try {
    const settings = await api("templates");
    if (request !== templateRequest) return;
    mailTemplates = settings;
    pickMailTemplate();
  } catch (error) {
    if (request === templateRequest) byId("templates-status").textContent = error.message;
  }
}

async function saveMailTemplates(items, nextIndex, message) {
  const request = templateRequest;
  const close = byId("close-templates");
  const actions = templatesForm.querySelectorAll("footer");
  close.disabled = true;
  actions.forEach(footer => { footer.inert = true; });
  byId("template-fields").inert = true;
  byId("templates-error").textContent = "";
  try {
    const settings = await api("templates", "PUT", { revision: mailTemplates.revision, templates: items });
    if (request !== templateRequest) return;
    mailTemplates = settings;
  } catch (error) {
    if (request === templateRequest) byId("templates-error").textContent = error.message;
    return;
  } finally {
    close.disabled = false;
    actions.forEach(footer => { footer.inert = false; });
    byId("template-fields").inert = false;
  }
  pickMailTemplate(nextIndex);
  byId("templates-status").textContent = message;
}

function insertMailTemplate() {
  const item = mailTemplates.templates[byId("template-choice").selectedIndex];
  const { body, subject } = editorForm.elements;
  const offset = body.selectionEnd;
  const before = body.value.slice(0, offset);
  const after = body.value.slice(offset);
  const prefix = before && !before.endsWith("\n") && !item.body.startsWith("\n") ? "\n" : "";
  const suffix = after && !after.startsWith("\n") && !item.body.endsWith("\n") ? "\n" : "";
  body.setRangeText(prefix + item.body + suffix, offset, offset, "end");
  body.dispatchEvent(new Event("input", { bubbles: true }));
  if (!subject.value.trim() && item.subject) subject.value = item.subject;
  closeMailTemplates();
  body.focus();
}

document.addEventListener("DOMContentLoaded", () => {
  byId("show-templates").addEventListener("click", () => void openMailTemplates());
  byId("close-templates").addEventListener("click", closeMailTemplates);
  templatesDialog.addEventListener("cancel", event => {
    event.preventDefault();
    if (!byId("close-templates").disabled) closeMailTemplates();
  });
  byId("template-choice").addEventListener("change", previewMailTemplate);
  byId("new-template").addEventListener("click", () => editMailTemplate(-1));
  byId("edit-template").addEventListener("click", () => editMailTemplate(byId("template-choice").selectedIndex));
  byId("insert-template").addEventListener("click", insertMailTemplate);
  byId("cancel-template-edit").addEventListener("click", () => pickMailTemplate(Math.max(0, editedTemplate)));
  byId("delete-template").addEventListener("click", () => {
    showTemplateView("delete");
    byId("templates-title").textContent = "删除模板";
    byId("templates-status").textContent = "删除「" + mailTemplates.templates[editedTemplate].name + "」？";
    byId("cancel-template-delete").focus();
  });
  byId("cancel-template-delete").addEventListener("click", () => {
    showTemplateView("edit");
    byId("templates-title").textContent = "编辑模板";
    byId("templates-status").textContent = "";
    byId("delete-template").focus();
  });
  byId("confirm-template-delete").addEventListener("click", () => {
    const items = mailTemplates.templates.filter((_, index) => index !== editedTemplate);
    void saveMailTemplates(items, editedTemplate, items.length ? "模板已删除" : "模板已删除。可以新建一份模板。");
  });
  templatesForm.addEventListener("submit", event => {
    event.preventDefault();
    if (byId("template-fields").hidden || byId("close-templates").disabled) return;
    const { templateName, templateSubject, templateBody } = templatesForm.elements;
    const items = [...mailTemplates.templates];
    const index = editedTemplate < 0 ? items.length : editedTemplate;
    items[index] = { name: templateName.value, subject: templateSubject.value, body: templateBody.value };
    void saveMailTemplates(items, index, "模板已保存");
  });
});
