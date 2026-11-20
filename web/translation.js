"use strict";

const translationDialog = document.getElementById("translation-dialog");
const translationForm = document.getElementById("translation-form");
const translateButton = document.getElementById("translate-letter");
const translationSubmit = translationForm.querySelector('[type="submit"]');
const translationTarget = translationForm.elements.target;
const translationStatus = document.getElementById("translation-status");
const translationResult = document.getElementById("translation-result");
const translationSummary = document.getElementById("translation-summary");
const translationCacheStatus = document.getElementById("translation-cache-status");
const translationSwitch = document.getElementById("translation-switch");
const translationSettings = document.getElementById("translation-settings");
const translationLanguageNames = new Intl.DisplayNames("zh-CN", { type: "language" });
const translationBody = document.getElementById("translation-body");
const translationProvider = translationForm.elements.provider;
const translationNotice = document.getElementById("translation-notice");
const translationSourceField = document.getElementById("translation-source-field");
const translationSource = translationForm.elements.source;
translationSource.replaceChildren(...Array.from(translationTarget.options, option => option.cloneNode(true)));
translationSource.value = "en";
const translationProviderNames = new Map([
  ["browser", "浏览器本地翻译"], ["microsoft", "微软翻译"], ["deepl", "DeepL"], ["google", "Google Cloud 翻译"], ["libretranslate", "LibreTranslate"]
]);
let translationRequest;
let browserTranslation;
let displayedTranslation;
let showTranslation = false;

function activeLetterTranslation() {
  return showTranslation ? displayedTranslation : undefined;
}

function updateInlineTranslation() {
  translationSwitch.textContent = showTranslation ? "显示原文" : "显示译文";
  if (state.current) renderBodyFormat(state.current);
}

function clearLetterTranslation() {
  closeLetterTranslation();
  displayedTranslation = undefined;
  showTranslation = false;
  translationResult.hidden = true;
  translationBody.hidden = true;
  translationBody.textContent = translationSummary.textContent = translationCacheStatus.textContent = "";
  translationBody.removeAttribute("lang");
}

function usesBrowserTranslation() {
  return translationProvider.value === "browser";
}

function updateTranslationNotice() {
  translationSourceField.hidden = !usesBrowserTranslation();
  if (usesBrowserTranslation()) {
    translationNotice.textContent = "邮件文本在浏览器处理。首次翻译可能下载语言模型，译文可在邮箱中复用。";
    return;
  }
  const service = translationProviderNames.get(translationProvider.value);
  translationNotice.textContent = service
    ? "未命中缓存时，邮件主题和正文文本会发送至 " + service + "。"
    : "";
}

function configureLetterTranslation(providers, allowBrowser) {
  const previous = translationProvider.value;
  const ids = providers.filter(id => id !== "browser" && translationProviderNames.has(id));
  if (allowBrowser && globalThis.isSecureContext && typeof globalThis.Translator?.create === "function") ids.unshift("browser");
  translationProvider.replaceChildren(...ids.map(id => new Option(translationProviderNames.get(id), id)));
  if (ids.includes(previous)) translationProvider.value = previous;
  updateTranslationNotice();
  return ids.length > 0;
}

function resetLetterTranslation() {
  translationRequest?.abort();
  translationRequest = undefined;
  browserTranslation = undefined;
  translationStatus.textContent = "";
  translationSubmit.disabled = translationTarget.disabled = translationProvider.disabled = translationSource.disabled = false;
}

function closeLetterTranslation() {
  resetLetterTranslation();
  if (translationDialog.open) translationDialog.close();
}

function openLetterTranslation() {
  if (translateButton.hidden || translationDialog.open) return;
  resetLetterTranslation();
  translationDialog.showModal();
  prepareBrowserTranslation();
}

translateButton.addEventListener("click", openLetterTranslation);
translationSettings.addEventListener("click", openLetterTranslation);
translationSwitch.addEventListener("click", () => {
  if (!displayedTranslation) return;
  showTranslation = !showTranslation;
  updateInlineTranslation();
});

document.getElementById("translation-close").addEventListener("click", closeLetterTranslation);
translationDialog.addEventListener("cancel", event => {
  event.preventDefault();
  closeLetterTranslation();
});
for (const field of [translationTarget, translationProvider, translationSource]) {
  field.addEventListener("change", () => {
    resetLetterTranslation();
    updateTranslationNotice();
    prepareBrowserTranslation();
  });
}

async function prepareBrowserTranslation() {
  if (!usesBrowserTranslation() || !state.current || translateButton.hidden) return;
  const source = translationSource.value;
  const target = translationTarget.value;
  translationSubmit.disabled = true;
  if (source === target) {
    translationStatus.textContent = "请选择不同的原文语言和目标语言。";
    return;
  }
  const path = "letters/" + state.current.id + "/translate-local?" + new URLSearchParams({ source, target });
  const request = new AbortController();
  translationRequest = request;
  translationStatus.textContent = "正在读取邮件…";
  try {
    const data = await api(path, "GET", undefined, {}, request.signal);
    if (translationRequest !== request) return;
    browserTranslation = { ...data, source, target, path };
    translationStatus.textContent = "";
  } catch (error) {
    if (translationRequest === request) translationStatus.textContent = error.message;
  } finally {
    if (translationRequest === request) {
      translationRequest = undefined;
      translationSubmit.disabled = false;
    }
  }
}

async function translateInBrowser(prepared, request) {
  const { cached, original, source, target } = prepared;
  if (cached) return cached;
  const { signal } = request;
  let translator;
  try {
    // Model creation must start in the submit gesture, before any network await.
    translator = await globalThis.Translator.create({
      sourceLanguage: source === "zh-Hans" ? "zh" : source,
      targetLanguage: target === "zh-Hans" ? "zh" : target,
      signal,
      monitor(model) {
        model.addEventListener("downloadprogress", event => {
          if (translationRequest !== request) return;
          const progress = Math.round(event.loaded * 100);
          translationStatus.textContent = progress < 100 ? "正在下载语言模型 " + progress + "%" : "正在准备翻译…";
        }, { signal });
      }
    });
    signal.throwIfAborted();
    translationStatus.textContent = "正在翻译…";
    const subject = original.subject ? await translator.translate(original.subject, { signal }) : "";
    const body = original.body ? await translator.translate(original.body, { signal }) : "";
    if ((original.subject && !subject.trim()) || (original.body && !body.trim())) throw new Error("Empty translation");
    return { subject, body };
  } catch (error) {
    const message = error.name === "NotSupportedError"
      ? "此浏览器暂不支持所选语言组合。"
      : "浏览器暂时无法完成翻译。请重试或切换翻译服务。";
    throw new Error(message);
  } finally {
    translator?.destroy();
  }
}
translationForm.addEventListener("submit", async event => {
  event.preventDefault();
  const letter = state.current;
  if (!letter || translateButton.hidden || translationRequest) return;
  const prepared = browserTranslation;
  if (usesBrowserTranslation() && !prepared) {
    prepareBrowserTranslation();
    return;
  }
  resetLetterTranslation();
  browserTranslation = prepared;
  const target = translationTarget.value;
  const request = new AbortController();
  translationRequest = request;
  translationSubmit.disabled = translationTarget.disabled = translationProvider.disabled = translationSource.disabled = true;
  translationStatus.textContent = "正在翻译…";
  try {
    const result = prepared
      ? await translateInBrowser(prepared, request)
      : await api("letters/" + letter.id + "/translate", "POST", { target, provider: translationProvider.value }, {}, request.signal);
    if (translationRequest !== request) return;
    let status = prepared?.status ?? "";
    if (prepared && !prepared.cached) {
      try {
        await api(prepared.path, "POST", { sourceVersion: prepared.sourceVersion, translation: result }, {}, request.signal);
      } catch (error) {
        if ([404, 409].includes(error.status)) {
          if (translationRequest === request) browserTranslation = undefined;
          throw error;
        }
        status = error.status === 413 ? "译文超出缓存大小。" : "译文暂时无法保存。";
      }
      if (translationRequest !== request) return;
      browserTranslation = { ...prepared, cached: result, status };
    }
    displayedTranslation = result;
    showTranslation = true;
    const language = translationLanguageNames.of(target);
    translationSummary.textContent = prepared
      ? "已翻译 · " + translationLanguageNames.of(prepared.source) + " → " + language
      : "已翻译为 " + language;
    translationCacheStatus.textContent = status;
    translationCacheStatus.hidden = !status;
    translationBody.lang = target;
    translationBody.textContent = result.body;
    translationResult.hidden = false;
    closeLetterTranslation();
    updateInlineTranslation();
    translationSwitch.focus({ preventScroll: true });
  } catch (error) {
    if (translationRequest === request) translationStatus.textContent = error.message;
  } finally {
    if (translationRequest === request) {
      translationRequest = undefined;
      translationSubmit.disabled = translationTarget.disabled = translationProvider.disabled = translationSource.disabled = false;
    }
  }
});
