"use strict";

async function downloadMailArchive(selection) {
  const response = await fetch("/api/mail-export", {
    method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(selection),
  });
  if (!response.ok) {
    if (response.status === 401) showLogin();
    const error = response.headers.get("Content-Type")?.includes("application/json") ? await response.json() : null;
    throw new Error(error?.error || "无法导出邮件");
  }
  const source = URL.createObjectURL(await response.blob());
  const link = document.createElement("a");
  link.href = source;
  link.download = "harbor-mail.zip";
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(source), 30000);
}

{
  const dialog = byId("mail-transfer-dialog");
  const importForm = byId("mail-import-form");
  const exportForm = byId("mail-export-form");
  const importError = byId("mail-import-error");
  const exportError = byId("mail-export-error");
  const close = byId("close-mail-transfer");
  const upload = byId("mail-import-files");
  const importFolder = byId("mail-import-folder");
  const exportFolder = byId("mail-export-folder");
  const submits = [importForm, exportForm].map(form => form.querySelector('[type="submit"]'));
  let pending = false;

  function folderOptions(select, keys) {
    const choices = [...keys.map(id => [id, folders[id]]), ...customFolders];
    select.replaceChildren(...choices.map(([id, name]) => new Option(name, id)));
    if (choices.some(([id]) => id === state.folder)) select.value = state.folder;
  }

  function setPending(value) {
    pending = value;
    for (const button of [...submits, close]) button.disabled = value;
  }

  byId("show-mail-transfer").addEventListener("click", () => {
    importForm.reset();
    importError.textContent = "";
    exportError.textContent = "";
    folderOptions(importFolder, ["inbox", "sent", "archive"]);
    folderOptions(exportFolder, ["inbox", "snoozed", "sent", "archive", "drafts", "junk", "trash"]);
    dialog.showModal();
  });
  close.addEventListener("click", () => dialog.close());
  dialog.addEventListener("cancel", event => { if (pending) event.preventDefault(); });
  upload.addEventListener("change", () => { importError.textContent = ""; });
  importForm.addEventListener("submit", async event => {
    event.preventDefault();
    if (pending) return;
    const files = [...upload.files];
    if (files.length > batchLimit || files.reduce((sum, file) => sum + file.size, 0) > 64 * 1024 * 1024) {
      importError.textContent = "每批最多 100 封、合计 64 MiB，请分批处理";
      return;
    }
    if (files.some(file => file.size > 10 * 1024 * 1024)) {
      importError.textContent = "单封 EML 邮件最多 10 MiB";
      return;
    }
    importError.textContent = "";
    const data = new FormData(importForm);
    setPending(true);
    try {
      const result = await api("mail-import", "POST", data);
      if (dialog.open) {
        dialog.close();
        toast("已导入 " + result.imported + " 封邮件");
        selectFolder(data.get("folder"));
      }
    } catch (err) {
      importError.textContent = err.message;
    } finally {
      setPending(false);
    }
  });
  exportForm.addEventListener("submit", async event => {
    event.preventDefault();
    if (pending) return;
    exportError.textContent = "";
    setPending(true);
    try {
      await downloadMailArchive({ folder: exportFolder.value });
      if (dialog.open) dialog.close();
      toast("已生成邮件 ZIP");
    } catch (err) {
      exportError.textContent = err.message;
    } finally {
      setPending(false);
    }
  });
}
