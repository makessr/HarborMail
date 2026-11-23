"use strict";

const systemTheme = matchMedia("(prefers-color-scheme: dark)");

function applySiteTheme(theme) {
  const root = document.documentElement;
  root.dataset.theme = theme === "dark" || (theme === "system" && systemTheme.matches) ? "dark" : "light";
  document.querySelector('meta[name="theme-color"]').content = getComputedStyle(root).backgroundColor;
}

function installNavigationDrawer(prefix, navigation) {
  const narrow = matchMedia("(max-width: 760px)");
  const drawer = document.getElementById(prefix + "-menu-drawer");
  const opener = document.getElementById(prefix + "-menu-toggle");
  const container = navigation.parentElement;
  const next = navigation.nextSibling;
  const close = () => { if (drawer.open) drawer.close(); };

  function sync() {
    if (narrow.matches) {
      drawer.append(navigation);
    } else {
      close();
      container.insertBefore(navigation, next);
    }
  }

  sync();
  narrow.addEventListener("change", sync);
  opener.addEventListener("click", () => {
    drawer.showModal();
    opener.setAttribute("aria-expanded", "true");
  });
  document.getElementById(prefix + "-menu-close").addEventListener("click", close);
  drawer.addEventListener("click", event => {
    if (event.target === drawer && event.clientX > drawer.getBoundingClientRect().right) close();
  });
  drawer.addEventListener("close", () => {
    opener.setAttribute("aria-expanded", "false");
    if (narrow.matches) opener.focus({ preventScroll: true });
  });
  return { close };
}

function installMailListResize(mailbox, rail) {
  const pane = document.getElementById("mail-list-pane");
  const divider = document.getElementById("mail-list-divider");
  const key = "harbor-mail-list-width";
  let preferredWidth = 0;
  let drag;

  function persist() {
    try {
      if (preferredWidth) localStorage.setItem(key, String(preferredWidth));
      else localStorage.removeItem(key);
    } catch {
      // Resizing remains available when browser storage is blocked.
    }
  }

  function updateWidth(width) {
    if (width) mailbox.style.setProperty("--mail-list-width", width + "px");
    else mailbox.style.removeProperty("--mail-list-width");
  }

  function bounds() {
    const style = getComputedStyle(mailbox);
    const min = parseFloat(style.getPropertyValue("--mail-list-min"));
    const available = mailbox.clientWidth - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight) -
      rail.offsetWidth - parseFloat(style.getPropertyValue("--mail-reading-min"));
    const max = Math.max(min, Math.min(parseFloat(style.getPropertyValue("--mail-list-max")), available));
    return { min, max };
  }

  function syncRange() {
    if (!divider.offsetWidth) return;
    const { min, max } = bounds();
    divider.setAttribute("aria-valuemin", String(min));
    divider.setAttribute("aria-valuemax", String(max));
    divider.setAttribute("aria-valuenow", String(pane.offsetWidth));
  }

  function resize(width, { min, max }) {
    const adjusted = Math.round(Math.min(max, Math.max(min, width)));
    updateWidth(adjusted);
    divider.setAttribute("aria-valuenow", String(adjusted));
    return adjusted;
  }

  try {
    const stored = Number(localStorage.getItem(key));
    if (Number.isSafeInteger(stored) && stored > 0 && stored <= 600) preferredWidth = stored;
  } catch {}
  updateWidth(preferredWidth);
  new ResizeObserver(syncRange).observe(mailbox);

  divider.addEventListener("pointerdown", event => {
    if (event.button !== 0 || !event.isPrimary) return;
    drag = { id: event.pointerId, start: event.clientX, width: pane.offsetWidth, limits: bounds() };
    divider.setPointerCapture(event.pointerId);
    mailbox.classList.add("resizing-mail-list");
  });
  divider.addEventListener("pointermove", event => {
    if (drag?.id !== event.pointerId) return;
    resize(drag.width + event.clientX - drag.start, drag.limits);
  });
  divider.addEventListener("pointerup", event => {
    if (drag?.id !== event.pointerId) return;
    if (event.clientX !== drag.start) {
      preferredWidth = resize(drag.width + event.clientX - drag.start, drag.limits);
      persist();
    } else {
      updateWidth(preferredWidth);
      syncRange();
    }
    drag = undefined;
    mailbox.classList.remove("resizing-mail-list");
  });
  divider.addEventListener("lostpointercapture", () => {
    if (!drag) return;
    drag = undefined;
    updateWidth(preferredWidth);
    mailbox.classList.remove("resizing-mail-list");
    syncRange();
  });
  divider.addEventListener("keydown", event => {
    const limits = bounds();
    const widths = { ArrowLeft: pane.offsetWidth - 24, ArrowRight: pane.offsetWidth + 24, Home: limits.min, End: limits.max };
    if (!Object.hasOwn(widths, event.key)) return;
    event.preventDefault();
    preferredWidth = resize(widths[event.key], limits);
    persist();
  });
  divider.addEventListener("dblclick", () => {
    preferredWidth = 0;
    updateWidth(0);
    persist();
    syncRange();
  });
}
