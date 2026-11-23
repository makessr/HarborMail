"use strict";

const twoFactorDialog = document.getElementById("two-factor-dialog");
const twoFactorForm = document.getElementById("two-factor-form");
let twoFactorRequest = 0;
let twoFactorSetupToken = "";
let twoFactorEnabled = false;

function clearTwoFactorSecrets() {
  twoFactorSetupToken = "";
  twoFactorForm.reset();
  byId("two-factor-key").value = "";
  byId("two-factor-qr").width = 0;
  byId("two-factor-codes").textContent = "";
  byId("two-factor-recovery").hidden = true;
  twoFactorForm.hidden = true;
}

function resetTwoFactor() {
  ++twoFactorRequest;
  clearTwoFactorSecrets();
  if (twoFactorDialog.open) twoFactorDialog.close();
}

async function refreshTwoFactor() {
  const request = ++twoFactorRequest;
  clearTwoFactorSecrets();
  const status = byId("two-factor-status");
  const actions = byId("two-factor-actions");
  actions.hidden = true;
  status.textContent = "正在读取两步验证状态…";
  try {
    const { enabled, managed, available } = await api("two-factor");
    if (request !== twoFactorRequest || !twoFactorDialog.open) return;
    twoFactorEnabled = enabled;
    status.textContent = managed ? "此账号的两步验证由服务器管理。" : enabled
      ? "两步验证已开启。邮件客户端请使用应用密码。"
      : available ? "两步验证未开启。开启后，网页登录需要认证器验证码，邮件客户端使用应用密码。"
        : "服务器尚未配置密钥保护，请联系管理员。";
    actions.hidden = managed || (!available && !enabled);
    byId("setup-two-factor").hidden = enabled;
    byId("disable-two-factor").hidden = !enabled;
  } catch (error) {
    if (request === twoFactorRequest && twoFactorDialog.open) status.textContent = error.message;
  }
}

function drawTwoFactorQR(modules) {
  const canvas = byId("two-factor-qr");
  canvas.width = canvas.height = modules.length;
  const context = canvas.getContext("2d");
  context.fillStyle = "#fff";
  context.fillRect(0, 0, canvas.width, canvas.height);
  context.fillStyle = "#111";
  modules.forEach((row, y) => row.forEach((visible, x) => {
    if (visible) context.fillRect(x, y, 1, 1);
  }));
}

function openTwoFactorForm(enable) {
  byId("two-factor-actions").hidden = true;
  byId("two-factor-setup").hidden = !enable;
  byId("two-factor-code-label").textContent = enable ? "认证器验证码" : "认证器验证码或恢复码";
  byId("submit-two-factor").textContent = enable ? "确认开启" : "确认关闭";
  twoFactorForm.elements.code.pattern = enable ? "[0-9]{6}" : "[0-9A-Fa-f]{32}|[0-9]{6}";
  twoFactorForm.hidden = false;
  (enable ? byId("two-factor-key") : twoFactorForm.elements.password).focus();
}

async function startTwoFactorSetup() {
  const button = byId("setup-two-factor");
  const status = byId("two-factor-status");
  const request = ++twoFactorRequest;
  button.disabled = true;
  try {
    const setup = await api("two-factor/setup", "POST");
    if (request !== twoFactorRequest || !twoFactorDialog.open) return;
    twoFactorSetupToken = setup.setup;
    drawTwoFactorQR(setup.qr);
    byId("two-factor-key").value = setup.secret;
    status.textContent = "扫码后，输入当前密码和认证器显示的六位验证码。";
    openTwoFactorForm(true);
  } catch (error) {
    if (request === twoFactorRequest && twoFactorDialog.open) status.textContent = error.message;
  } finally {
    button.disabled = false;
  }
}

document.addEventListener("DOMContentLoaded", () => {
  byId("show-two-factor").addEventListener("click", () => {
    twoFactorDialog.showModal();
    void refreshTwoFactor();
  });
  byId("close-two-factor").addEventListener("click", resetTwoFactor);
  byId("finish-two-factor").addEventListener("click", resetTwoFactor);
  byId("cancel-two-factor").addEventListener("click", () => void refreshTwoFactor());
  twoFactorDialog.addEventListener("close", () => {
    if (!twoFactorDialog.open) {
      ++twoFactorRequest;
      clearTwoFactorSecrets();
    }
  });
  byId("setup-two-factor").addEventListener("click", () => void startTwoFactorSetup());
  byId("disable-two-factor").addEventListener("click", () => {
    byId("two-factor-status").textContent = "关闭后，网页登录将恢复使用邮箱密码。";
    openTwoFactorForm(false);
  });
  twoFactorForm.addEventListener("submit", async event => {
    event.preventDefault();
    if (twoFactorForm.inert) return;
    const status = byId("two-factor-status");
    const request = twoFactorRequest;
    const { password, code } = twoFactorForm.elements;
    const payload = { password: password.value, code: code.value.trim(), setup: twoFactorSetupToken };
    twoFactorForm.inert = true;
    try {
      const result = await api("two-factor", twoFactorEnabled ? "DELETE" : "POST", payload);
      if (request !== twoFactorRequest || !twoFactorDialog.open) return;
      clearTwoFactorSecrets();
      if (result.enabled) {
        status.textContent = "两步验证已开启，其他浏览器已退出。";
        byId("two-factor-codes").textContent = result.recoveryCodes.join("\n");
        byId("two-factor-recovery").hidden = false;
        byId("finish-two-factor").focus();
      } else {
        await refreshTwoFactor();
      }
    } catch (error) {
      if (request !== twoFactorRequest || !twoFactorDialog.open) return;
      if (error.status === 409) await refreshTwoFactor();
      status.textContent = error.message;
    } finally {
      twoFactorForm.inert = false;
    }
  });
});
