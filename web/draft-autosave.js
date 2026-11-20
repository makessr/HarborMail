"use strict";

const draftIdleDelay = 1700;
let composeDraft = null;

function warnUnsavedDraft(event) {
  event.preventDefault();
  event.returnValue = "";
}

function beginDraftTracking(existing) {
  resetDraftTracking();
  composeDraft = {
    saved: existing ? { id: existing.id, draftVersion: existing.draftVersion } : null,
    creationKey: Array.from(crypto.getRandomValues(new Uint8Array(16)), byte => byte.toString(16).padStart(2, "0")).join(""),
    edit: 0, savedEdit: 0, timer: 0, pending: null, firstSnapshot: null, error: null, busy: false, composing: false,
    initial: draftEditableValues(),
  };
  renderDraftTracking();
}

function resetDraftTracking() {
  clearTimeout(composeDraft?.timer);
  composeDraft = null;
  editorForm.inert = false;
  renderDraftTracking();
}

function draftFailureText(failure) {
  return failure instanceof TypeError ? "连接失败，请联网后重试。" : failure.message;
}

function hasDraftEdits(draft) {
  if (draft.edit === draft.savedEdit) return false;
  if (draft.saved || draft.firstSnapshot) return true;
  const current = draftEditableValues();
  return current.length !== draft.initial.length || current.some((value, index) => value !== draft.initial[index]);
}

function draftEditableValues() {
  const { to, cc, bcc, subject, body, author, formatted } = editorForm.elements;
  return [to.value, cc.value, bcc.value, subject.value, body.value, author.value, formatted.checked, ...selectedFiles];
}

function renderDraftTracking() {
  const draft = composeDraft;
  const dirty = draft && hasDraftEdits(draft);
  const status = byId("draft-save-status");
  const message = draft?.pending ? "正在保存草稿…" : draft?.error ? "草稿未保存。 " + draftFailureText(draft.error) :
    dirty ? "有未保存的更改" : draft?.saved ? "草稿已保存" : "";
  if (dirty) window.addEventListener("beforeunload", warnUnsavedDraft);
  else window.removeEventListener("beforeunload", warnUnsavedDraft);
  status.hidden = !message;
  status.toggleAttribute("data-failed", Boolean(draft?.error && !draft.pending));
  byId("draft-save-message").textContent = message;
  byId("discard-draft-edits").hidden = !draft?.error || Boolean(draft.pending);
}

function queueDraftSave(draft) {
  clearTimeout(draft.timer);
  if (draft.busy || draft.pending || draft.composing || !hasDraftEdits(draft) || draft.error?.status === 409) return;
  draft.timer = setTimeout(() => {
    if (composeDraft === draft && editor.open) void persistDraftSnapshot(draft).catch(() => {});
  }, draftIdleDelay);
}

function markDraftEdited() {
  if (!composeDraft || composeDraft.busy) return;
  composeDraft.edit++;
  renderDraftTracking();
  queueDraftSave(composeDraft);
}

function persistDraftSnapshot(draft) {
  if (draft.pending) return draft.pending;
  const snapshot = draft.firstSnapshot || { edit: draft.edit, payload: composePayload() };
  if (!draft.saved) draft.firstSnapshot = snapshot;
  const request = draft.saved
    ? api("drafts/" + draft.saved.id, "PUT", snapshot.payload, { "If-Match": String(draft.saved.draftVersion) })
    : api("drafts", "POST", snapshot.payload, { "Idempotency-Key": draft.creationKey });
  draft.pending = request.then(saved => {
    draft.saved = saved;
    draft.savedEdit = snapshot.edit;
    draft.firstSnapshot = null;
    draft.error = null;
  }).catch(error => {
    if (error.status >= 400 && error.status < 500) draft.firstSnapshot = null;
    draft.error = error;
    throw error;
  }).finally(() => {
    draft.pending = null;
    if (composeDraft === draft) {
      renderDraftTracking();
      if (!draft.error || (draft.error.status < 500 && draft.edit !== snapshot.edit)) queueDraftSave(draft);
    }
  });
  renderDraftTracking();
  return draft.pending;
}

async function flushDraftEdits(draft, forceSave = false) {
  if (draft.pending) await draft.pending;
  while (composeDraft === draft && (hasDraftEdits(draft) || (forceSave && !draft.saved))) {
    await persistDraftSnapshot(draft);
  }
}

function closeTrackedEditor() {
  resetDraftTracking();
  editor.close();
}

async function finishCompose(action) {
  const draft = composeDraft;
  if (!draft || draft.busy || draft.composing) return;
  draft.busy = true;
  clearTimeout(draft.timer);
  const focused = document.activeElement;
  const errorOutput = byId("send-error");
  editorForm.inert = true;
  errorOutput.textContent = "";
  try {
    let result;
    if (action === "send") {
      const schedule = composeScheduleQuery();
      if (draft.saved || draft.pending || draft.firstSnapshot) {
        await flushDraftEdits(draft);
        if (composeDraft !== draft) return;
        result = await api("drafts/" + draft.saved.id + "/send" + schedule, "POST", undefined,
          { "If-Match": String(draft.saved.draftVersion) });
      } else {
        result = await api("letters" + schedule, "POST", composePayload());
      }
    } else {
      await flushDraftEdits(draft, action === "save");
    }
    if (composeDraft !== draft) return;
    closeTrackedEditor();
    if (action === "save") {
      toast("草稿已保存");
      void selectFolder("drafts");
    } else if (action === "send") {
      if (draft.saved) clearReading();
      if (result && Object.hasOwn(result, "undoRemainingMs")) {
        showUndoSend(result);
        await load(false, true);
      } else if (result?.sendAt) {
        toast("已安排在 " + fullDate.format(result.sendAt * 1000) + " 发送");
        void selectFolder("outbox");
      } else {
        toast(result?.queued ? "邮件已加入发件箱" : "邮件已发送");
        await load();
      }
    }
  } catch (error) {
    if (composeDraft === draft && error !== draft.error) errorOutput.textContent = draftFailureText(error);
  } finally {
    if (composeDraft === draft) {
      editorForm.inert = false;
      draft.busy = false;
      focused?.focus({ preventScroll: true });
      if (!draft.error) queueDraftSave(draft);
    }
  }
}

document.addEventListener("DOMContentLoaded", () => {
  editorForm.addEventListener("input", event => {
    if (["to", "cc", "bcc", "subject", "body", "author", "formatted"].includes(event.target.name)) markDraftEdited();
  });
  editorForm.addEventListener("compositionstart", () => {
    if (!composeDraft) return;
    composeDraft.composing = true;
    clearTimeout(composeDraft.timer);
  });
  editorForm.addEventListener("compositionend", () => {
    if (!composeDraft) return;
    composeDraft.composing = false;
    queueDraftSave(composeDraft);
  });
  window.addEventListener("online", () => { if (composeDraft) queueDraftSave(composeDraft); });
  byId("save-draft").addEventListener("click", () => void finishCompose("save"));
  byId("close-editor").addEventListener("click", () => void finishCompose("close"));
  byId("discard-draft-edits").addEventListener("click", closeTrackedEditor);
  editor.addEventListener("cancel", event => {
    event.preventDefault();
    void finishCompose("close");
  });
});
