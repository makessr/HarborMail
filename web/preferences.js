"use strict";

const defaultMailboxPreferences = Object.freeze({ revision: 0, theme: "light", density: "comfortable", readMode: "auto", undoSeconds: 0 });
const preferencesDialog = document.getElementById("preferences-dialog");
const preferencesForm = document.getElementById("preferences-form");
let activeMailboxPreferences = defaultMailboxPreferences;
let preferencesSession = 0;
let preferencesRevision = 0;

function displayMailboxPreferences(prefs) {
  applySiteTheme(prefs.theme);
  document.documentElement.dataset.density = prefs.density;
}

function acceptMailboxPreferences(prefs) {
  if (prefs.revision < activeMailboxPreferences.revision) return;
  activeMailboxPreferences = prefs;
  updateComposeUndoHint();
  if (!preferencesDialog.open) displayMailboxPreferences(prefs);
}

async function refreshMailboxPreferences() {
  const session = preferencesSession;
  try {
    const prefs = await api("preferences");
    if (session === preferencesSession) acceptMailboxPreferences(prefs);
  } catch {
    // A transient failure leaves the last known preferences active.
  }
  return session === preferencesSession;
}

function selectedMailboxPreferences() {
  return {
    revision: preferencesRevision,
    theme: preferencesForm.elements.theme.value,
    density: preferencesForm.elements.density.value,
    readMode: preferencesForm.elements.autoRead.checked ? "auto" : "manual",
    undoSeconds: Number(preferencesForm.elements.undoSeconds.value),
  };
}

function populateMailboxPreferences(prefs) {
  preferencesForm.elements.theme.value = prefs.theme;
  preferencesForm.elements.density.value = prefs.density;
  preferencesForm.elements.autoRead.checked = prefs.readMode === "auto";
  preferencesForm.elements.undoSeconds.value = String(prefs.undoSeconds || 0);
}

function closeMailboxPreferences() {
  if (preferencesDialog.open) preferencesDialog.close();
  byId("preferences-error").textContent = "";
  displayMailboxPreferences(activeMailboxPreferences);
}

function resetMailboxPreferences() {
  ++preferencesSession;
  activeMailboxPreferences = defaultMailboxPreferences;
  updateComposeUndoHint();
  closeMailboxPreferences();
}

async function openMailboxPreferences() {
  if (!await refreshMailboxPreferences() || byId("mailbox").hidden || preferencesDialog.open) return;
  preferencesRevision = activeMailboxPreferences.revision;
  populateMailboxPreferences(activeMailboxPreferences);
  preferencesDialog.showModal();
  void refreshWebPushPreference();
}

systemTheme.addEventListener("change", () => {
  displayMailboxPreferences(preferencesDialog.open ? selectedMailboxPreferences() : activeMailboxPreferences);
});

document.addEventListener("DOMContentLoaded", () => {
  byId("show-preferences").addEventListener("click", () => void openMailboxPreferences());
  byId("close-preferences").addEventListener("click", closeMailboxPreferences);
  preferencesDialog.addEventListener("cancel", event => {
    event.preventDefault();
    if (!byId("close-preferences").disabled) closeMailboxPreferences();
  });
  preferencesForm.addEventListener("change", () => displayMailboxPreferences(selectedMailboxPreferences()));
  byId("default-preferences").addEventListener("click", () => {
    populateMailboxPreferences(defaultMailboxPreferences);
    displayMailboxPreferences(defaultMailboxPreferences);
  });
  preferencesForm.addEventListener("submit", async event => {
    event.preventDefault();
    const close = byId("close-preferences");
    if (close.disabled) return;
    const session = preferencesSession;
    const editing = selectedMailboxPreferences();
    const controls = preferencesForm.querySelectorAll("button, input, select");
    const error = byId("preferences-error");
    controls.forEach(control => { control.disabled = true; });
    error.textContent = "";
    try {
      const saved = await api("preferences", "PUT", editing);
      if (session !== preferencesSession) return;
      acceptMailboxPreferences(saved);
      closeMailboxPreferences();
      toast("邮箱偏好已保存");
    } catch (failure) {
      if (session === preferencesSession) error.textContent = failure.message;
    } finally {
      controls.forEach(control => { control.disabled = false; });
    }
  });
});
