"use strict";

const signaturesDialog = document.getElementById("signatures-dialog");
const signaturesForm = document.getElementById("signatures-form");
const signatureModes = ["compose", "reply", "forward"];
let workingSignatures = null;
let activeProfile = "";
let signatureSettingsRequest = 0;

function signatureOptions(items, emptyLabel) {
  return [new Option(emptyLabel, ""), ...items.map(item => new Option(item.name || "未命名签名", item.id))];
}

function renderSignatureChoices() {
  const { signatures, defaults } = workingSignatures;
  const picker = byId("signature-profile");
  picker.replaceChildren(...signatureOptions(signatures, "选择要编辑的签名"));
  picker.value = activeProfile;
  for (const mode of signatureModes) {
    const select = byId("signature-" + mode);
    select.replaceChildren(...signatureOptions(signatures, "不自动插入"));
    select.value = defaults[mode];
  }
  byId("new-signature").disabled = signatures.length >= 20;
}

function editSignatureProfile(id) {
  const profile = workingSignatures.signatures.find(item => item.id === id);
  activeProfile = profile?.id || "";
  const fields = byId("signature-fields");
  fields.hidden = fields.disabled = !profile;
  byId("signature-name").value = profile?.name || "";
  byId("signature-text").value = profile?.body || "";
  renderSignatureChoices();
}

function closeSignatures() {
  ++signatureSettingsRequest;
  workingSignatures = null;
  if (signaturesDialog.open) signaturesDialog.close();
  signaturesForm.reset();
  byId("signature-profile").replaceChildren();
  for (const mode of signatureModes) byId("signature-" + mode).replaceChildren();
}

async function openSignatures() {
  const request = ++signatureSettingsRequest;
  byId("signature-controls").hidden = true;
  byId("save-signatures").disabled = true;
  byId("signatures-error").textContent = "";
  byId("signatures-status").textContent = "正在读取签名…";
  signaturesDialog.showModal();
  try {
    const settings = await api("signatures");
    if (request !== signatureSettingsRequest) return;
    workingSignatures = settings;
    editSignatureProfile(settings.signatures[0]?.id || "");
    byId("signatures-status").textContent = "";
    byId("signature-controls").hidden = false;
    byId("save-signatures").disabled = false;
  } catch (error) {
    if (request === signatureSettingsRequest) byId("signatures-status").textContent = error.message;
  }
}

function insertSignatureProfile() {
  const id = Array.from(crypto.getRandomValues(new Uint8Array(16)), byte => byte.toString(16).padStart(2, "0")).join("");
  workingSignatures.signatures.push({ id, name: "", body: "" });
  editSignatureProfile(id);
  byId("signature-name").focus();
}

function removeSignatureProfile() {
  workingSignatures.signatures = workingSignatures.signatures.filter(item => item.id !== activeProfile);
  for (const mode of signatureModes) {
    if (workingSignatures.defaults[mode] === activeProfile) workingSignatures.defaults[mode] = "";
  }
  editSignatureProfile(workingSignatures.signatures[0]?.id || "");
}

let composeProfiles = [];
let signatureBeforeQuote = false;
let insertedSignature = null;
let trackedSignatureBody = "";

function resetComposeSignatures() {
  composeProfiles = [];
  insertedSignature = null;
  trackedSignatureBody = "";
  byId("compose-signature").replaceChildren();
  byId("compose-signature-row").hidden = true;
}

function prepareComposeSignature(settings, mode, beforeQuote) {
  composeProfiles = settings?.signatures || [];
  signatureBeforeQuote = beforeQuote;
  insertedSignature = null;
  trackedSignatureBody = editorForm.elements.body.value;
  const picker = byId("compose-signature");
  picker.replaceChildren(...signatureOptions(composeProfiles, "不使用签名"));
  byId("compose-signature-row").hidden = composeProfiles.length === 0;
  picker.value = settings?.defaults[mode] || "";
  replaceComposeSignature(picker.value);
  editorForm.elements.body.setSelectionRange(0, 0);
}

function trackSignatureInput() {
  const text = editorForm.elements.body.value;
  if (insertedSignature) {
    const { start, block } = insertedSignature;
    const originalTail = trackedSignatureBody.slice(start);
    if (text.endsWith(originalTail)) insertedSignature.start = text.length - originalTail.length;
    else if (!text.startsWith(block, start)) {
      // Hand edits belong to the message, so future selections leave them alone.
      insertedSignature = null;
      byId("compose-signature").value = "";
    }
  }
  trackedSignatureBody = text;
}

function replaceComposeSignature(id) {
  const body = editorForm.elements.body;
  let text = body.value;
  if (insertedSignature && text.startsWith(insertedSignature.block, insertedSignature.start)) {
    const { start, block } = insertedSignature;
    text = text.slice(0, start) + text.slice(start + block.length);
  }
  insertedSignature = null;
  const profile = composeProfiles.find(item => item.id === id);
  if (profile) {
    const quote = signatureBeforeQuote ? text.indexOf(forwardDivider) : -1;
    const start = quote < 0 ? text.length : quote;
    const prefix = text.slice(0, start);
    const block = "\n\n-- \n" + profile.body;
    if (!prefix.endsWith(block)) {
      insertedSignature = { start, block };
      text = prefix + block + text.slice(start);
    }
  }
  body.value = trackedSignatureBody = text;
}

document.addEventListener("DOMContentLoaded", () => {
  byId("show-signatures").addEventListener("click", () => void openSignatures());
  byId("close-signatures").addEventListener("click", closeSignatures);
  signaturesDialog.addEventListener("cancel", event => {
    event.preventDefault();
    if (!byId("close-signatures").disabled) closeSignatures();
  });
  byId("new-signature").addEventListener("click", insertSignatureProfile);
  byId("delete-signature").addEventListener("click", removeSignatureProfile);
  byId("signature-profile").addEventListener("change", event => editSignatureProfile(event.target.value));
  byId("signature-name").addEventListener("input", event => {
    workingSignatures.signatures.find(item => item.id === activeProfile).name = event.target.value;
    renderSignatureChoices();
  });
  byId("signature-text").addEventListener("input", event => {
    workingSignatures.signatures.find(item => item.id === activeProfile).body = event.target.value;
  });
  for (const mode of signatureModes) {
    byId("signature-" + mode).addEventListener("change", event => { workingSignatures.defaults[mode] = event.target.value; });
  }
  byId("compose-signature").addEventListener("change", event => {
    replaceComposeSignature(event.target.value);
    markDraftEdited();
  });
  editorForm.elements.body.addEventListener("input", trackSignatureInput);
  signaturesForm.addEventListener("submit", async event => {
    event.preventDefault();
    const save = byId("save-signatures"), close = byId("close-signatures");
    if (!workingSignatures || save.disabled) return;
    const request = signatureSettingsRequest;
    save.disabled = close.disabled = true;
    byId("signature-controls").inert = true;
    byId("signatures-error").textContent = "";
    try {
      await api("signatures", "PUT", workingSignatures);
      if (request === signatureSettingsRequest) {
        closeSignatures();
        toast("签名设置已保存");
      }
    } catch (error) {
      if (request === signatureSettingsRequest) byId("signatures-error").textContent = error.message;
    } finally {
      save.disabled = close.disabled = false;
      byId("signature-controls").inert = false;
    }
  });
});
