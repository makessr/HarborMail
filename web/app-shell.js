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
