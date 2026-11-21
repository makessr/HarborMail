"use strict";

const adminElement = id => document.getElementById(id);
const domainAdminOnly = document.body.hasAttribute("data-domain-admin");
if (domainAdminOnly) {
  for (const page of ["overview", "queue", "runtime", "storage", "security", "quarantine", "audit"]) {
    adminElement("admin-" + page + "-link").hidden = true;
  }
  adminElement("admin-domain-add").hidden = true;
}
const adminMenu = installNavigationDrawer("admin", adminElement("admin-nav"));
let adminTheme = "light";
systemTheme.addEventListener("change", () => applySiteTheme(adminTheme));
async function refreshAdminTheme() {
  try {
    const response = await fetch("/api/preferences");
    if (response.ok) {
      adminTheme = (await response.json()).theme;
      applySiteTheme(adminTheme);
    }
  } catch {
    // Keep the current theme if preferences are temporarily unavailable.
  }
}

const domainFromLink = new URLSearchParams(location.search).get("domain");
const showDomains = location.pathname === "/admin/domains";
const showQueue = location.pathname === "/admin/queue";
const showDelivery = location.pathname === "/admin/delivery";
const showDMARC = location.pathname === "/admin/dmarc";
const showAudit = location.pathname === "/admin/audit";
const showAliases = location.pathname === "/admin/aliases";
const showRuntime = location.pathname === "/admin/runtime";
const showStorage = location.pathname === "/admin/storage";
const showQuarantine = location.pathname === "/admin/quarantine";
const showSecurity = location.pathname === "/admin/security";
const showAccounts = location.pathname === "/admin/accounts" ||
  ((location.pathname === "/admin" || location.pathname === "/admin/") && Boolean(domainFromLink));
const section = showDelivery ? "delivery" : showDMARC ? "dmarc" : showQuarantine ? "quarantine" : showStorage ? "storage" : showSecurity ? "security" : showRuntime ? "runtime" : showAliases ? "aliases" : showAudit ? "audit" : showQueue ? "queue" :
  showDomains ? "domains" : showAccounts ? "accounts" : "overview";
const activePanel = adminElement({
  audit: "admin-audit-view", queue: "admin-delivery", domains: "admin-domains", accounts: "admin-account-view",
  aliases: "admin-alias-view", runtime: "admin-runtime-view", storage: "admin-storage-view",
  quarantine: "admin-quarantine-view", security: "admin-security-view", overview: "admin-overview",
  delivery: "admin-delivery-tools", dmarc: "admin-dmarc-tools"
}[section]);
const statusOutput = adminElement("admin-status");
const refresh = adminElement("admin-refresh");
const find = adminElement("admin-find");
const logout = adminElement("admin-logout");
const accountDomain = adminElement("admin-account-domain");
const accountRole = adminElement("admin-account-role");
const accountStatus = adminElement("admin-account-status");
const dnsDomain = adminElement("admin-dns-domain");
const dnsSelector = adminElement("admin-dns-selector");
const dnsCheck = adminElement("admin-dns-check");
const dnsStatus = adminElement("admin-dns-status");
const dnsResults = adminElement("admin-dns-results");
const queueLabels = {
  queued: "排队中", scheduled: "定时发送", undo: "撤销窗口",
  preparing: "准备投递", sending: "投递中", failed: "投递失败", uncertain: "结果待确认"
};
let accounts = [];
let signedInAdmin = "";
const domainOf = address => address.slice(address.lastIndexOf("@") + 1).replace(/\.$/, "").toLowerCase();
const navSection = showDelivery ? "queue" : showDMARC ? "domains" : section;
adminElement("admin-" + navSection + "-link").setAttribute("aria-current", "page");
const related = { queue: ["/admin/delivery", "投递日志与额度"], delivery: ["/admin/queue", "投递队列"],
  domains: ["/admin/dmarc", "DMARC 报告"], dmarc: ["/admin/domains", "域名管理"] }[section];
if (related && !domainAdminOnly) {
  const link = adminElement("admin-related-page");
  [link.href, link.textContent] = related;
  link.hidden = false;
}
if (section !== "overview") {
  const title = { queue: "投递队列", domains: "域名", accounts: "邮箱账号", audit: "操作审计",
    aliases: "别名管理", runtime: "运行监控", storage: "存储用量", security: "安全", quarantine: "隔离邮件", delivery: "投递日志", dmarc: "DMARC 报告" }[section];
  adminElement("admin-title").textContent = title;
  document.title = title + " · Harbor Mail";
}

function renderDetails(id, entries) {
  const content = document.createDocumentFragment();
  for (const [label, value] of entries) {
    const term = document.createElement("dt");
    term.textContent = label;
    const detail = document.createElement("dd");
    if (value instanceof HTMLElement) detail.append(value);
    else detail.textContent = value;
    content.append(term, detail);
  }
  adminElement(id).replaceChildren(content);
}

const accountList = adminElement("admin-accounts");
const accountSheet = adminElement("admin-account-sheet");
const accountSheetAccess = adminElement("admin-account-sheet-access");
const accountSheetLifecycle = adminElement("admin-account-sheet-lifecycle");
const accountRoleLabels = { admin: "全站管理员", domain_admin: "域管理员", user: "普通用户" };
const accountStatusLabels = { retired: "已回收", suspended: "已停用", active: "启用" };
const accountRoleOf = account => account.admin ? "admin" : account.domainAdmin ? "domain_admin" : "user";
const accountStatusOf = account => account.retired ? "retired" : account.suspended ? "suspended" : "active";
let selectedAccountEmail = "";

function renderAccountSheet(account) {
  adminElement("admin-account-sheet-email").textContent = account.email;
  renderDetails("admin-account-sheet-details", [
    ["角色", accountRoleLabels[accountRoleOf(account)]],
    ["状态", accountStatusLabels[accountStatusOf(account)]],
    ["来源", account.managed ? "后台" : "服务器配置"],
    ["别名", account.aliases?.join("、") || "无"]
  ]);
  adminElement("admin-account-sheet-actions").hidden = domainAdminOnly;
  accountSheetAccess.hidden = account.retired;
  const ownInstallationAdmin = account.configuredAdmin && account.email === signedInAdmin;
  accountSheetLifecycle.hidden = account.admin && !account.configuredAdmin;
  accountSheetLifecycle.textContent = ownInstallationAdmin ? "交接并回收" : account.admin ? "交接与删除" : "邮箱管理";
  adminElement("admin-account-sheet-storage").href = "/admin/storage?" + new URLSearchParams({ owner: account.email });
}

function renderAccounts() {
  if (!showAccounts) return;
  const query = find.value.trim().toLowerCase();
  const matching = accounts.filter(account => {
    const addresses = [account.email, ...(account.aliases || [])];
    return (!accountRole.value || accountRole.value === accountRoleOf(account)) &&
      (!accountStatus.value || accountStatus.value === accountStatusOf(account)) &&
      addresses.some(address => !accountDomain.value || domainOf(address) === accountDomain.value) &&
      addresses.some(address => address.toLowerCase().includes(query));
  });
  const content = document.createDocumentFragment();
  for (const account of matching) {
    const row = document.createElement("tr");
    const button = document.createElement("button");
    button.type = "button";
    button.className = "admin-account-open";
    button.dataset.email = account.email;
    button.textContent = account.email;
    button.setAttribute("aria-haspopup", "dialog");
    button.addEventListener("click", () => {
      selectedAccountEmail = account.email;
      renderAccountSheet(account);
      accountSheet.showModal();
    });
    row.insertCell().append(button);
    row.insertCell().textContent = accountRoleLabels[accountRoleOf(account)];
    const status = document.createElement("span");
    status.className = "admin-account-state";
    status.dataset.state = accountStatusOf(account);
    status.textContent = accountStatusLabels[status.dataset.state];
    row.insertCell().append(status);
    content.append(row);
  }
  accountList.replaceChildren(content);
  adminElement("admin-empty").hidden = matching.length !== 0;
  adminElement("admin-account-count").textContent = matching.length === accounts.length
    ? "共 " + accounts.length + " 个账号"
    : "显示 " + matching.length + " / " + accounts.length + " 个账号";
  if (accountSheet.open) {
    const account = accounts.find(item => item.email === selectedAccountEmail);
    if (account) renderAccountSheet(account);
    else accountSheet.close();
  }
}

function renderDomains(domains) {
  const previousFilter = accountDomain.options.length ? accountDomain.value : domainFromLink;
  const previousCheck = dnsDomain.value;
  accountDomain.replaceChildren(new Option("全部域名", ""), ...domains.map(({ name }) => new Option(name, name)));
  dnsDomain.replaceChildren(...domains.map(({ name }) => new Option(name, name)));
  if (domains.some(({ name }) => name === previousFilter)) accountDomain.value = previousFilter;
  if (domains.some(({ name }) => name === previousCheck)) dnsDomain.value = previousCheck;
  dnsCheck.disabled = dnsDomain.disabled || domains.length === 0;
  if (showDomains) renderDomainRows(domains);
}

async function loadOverview() {
  refresh.disabled = true;
  statusOutput.textContent = "正在读取管理数据…";
  try {
    const response = await fetch(section === "overview" ? "/api/admin/overview" : "/api/admin/accounts");
    if (response.status === 401 || response.status === 403) {
      location.replace("/");
      return;
    }
    if (!response.ok) throw new Error("无法读取管理数据，请刷新重试。");
    const data = await response.json();
    signedInAdmin = data.email;
    adminElement("admin-identity").textContent = data.email;
    if (section === "overview") {
      renderAdminOverview(data);
      renderOnboarding();
    }
    accounts = data.accounts;
    renderDomains(data.domains);
    renderAccounts();
    activePanel.hidden = false;
    statusOutput.textContent = "";
    if (showDomains && !domainAdminOnly) {
      const url = new URL(location.href);
      if (url.searchParams.get("create") === "1") {
        url.searchParams.delete("create");
        history.replaceState(null, "", url);
        openDomainEditor();
      } else if (url.hash === "#admin-dns-title") {
        adminElement("admin-dns-title").scrollIntoView({ block: "start" });
      }
    }
    if (showAccounts && domainFromLink && data.domains.some(domain => domain.name === domainFromLink)) {
      const url = new URL(location.href);
      if (url.searchParams.get("create") === "1") {
        url.searchParams.delete("create");
        history.replaceState(null, "", url);
        openAccountCreate(domainFromLink);
      }
    }
  } catch {
    statusOutput.textContent = "无法读取管理数据，请刷新重试。";
  } finally {
    refresh.disabled = false;
  }
}

const checkUpdate = adminElement("admin-check-update");
checkUpdate.addEventListener("click", async () => {
  const status = adminElement("admin-update-status");
  const release = adminElement("admin-latest-release");
  checkUpdate.disabled = true;
  release.hidden = true;
  release.removeAttribute("href");
  status.textContent = "正在检查更新…";
  try {
    const response = await fetch("/api/admin/about/update");
    if (!response.ok) throw new Error("Update check failed");
    const result = await response.json();
    const messages = {
      available: "发现新版本 " + result.latest,
      current: "已是最新稳定版 " + result.latest,
      ahead: "当前版本高于已发布的稳定版 " + result.latest,
      development: "最新稳定版 " + result.latest + "。开发构建无法判断是否需要升级。"
    };
    status.textContent = messages[result.status];
    release.href = "https://github.com/makessr/HarborMail/releases/tag/" + encodeURIComponent(result.latest);
    release.hidden = false;
  } catch {
    status.textContent = "暂时无法检查更新，请稍后重试。";
  } finally {
    checkUpdate.disabled = false;
  }
});

const onboarding = adminElement("admin-onboarding");
const onboardingShow = adminElement("admin-onboarding-show");
const onboardingHide = adminElement("admin-onboarding-hide");
let onboardingHidden = false;

function renderOnboarding() {
  try { onboardingHidden = localStorage.getItem("harbor-admin-guide:" + signedInAdmin) === "hidden"; } catch {}
  onboarding.hidden = onboardingHidden;
  onboardingShow.hidden = !onboardingHidden;
}

function setOnboardingHidden(hidden) {
  onboardingHidden = hidden;
  try { localStorage.setItem("harbor-admin-guide:" + signedInAdmin, hidden ? "hidden" : "shown"); } catch {}
  onboarding.hidden = hidden;
  onboardingShow.hidden = !hidden;
  (hidden ? onboardingShow : onboardingHide).focus();
}
onboardingHide.addEventListener("click", () => setOnboardingHidden(true));
onboardingShow.addEventListener("click", () => setOnboardingHidden(false));


function passwordConfirmationError(password, confirmation) {
  if (password.value !== confirmation.value) {
    confirmation.focus();
    return "两次输入的新密码不一致。";
  }
  const length = new TextEncoder().encode(password.value).length;
  if (length < 12 || length > 72) {
    password.focus();
    return "新密码长度须为 12 至 72 字节。";
  }
  return "";
}

{
  const dialog = adminElement("admin-password-dialog");
  const form = adminElement("admin-password-form");
  const { password, newPassword, confirmPassword } = form.elements;
  let busy = false;
  adminElement("admin-password-open").addEventListener("click", () => dialog.showModal());
  adminElement("admin-password-close").addEventListener("click", () => dialog.close());
  dialog.addEventListener("cancel", event => {
    if (busy) event.preventDefault();
  });
  dialog.addEventListener("close", () => {
    form.reset();
  });
  form.addEventListener("submit", async event => {
    event.preventDefault();
    if (busy) return;
    const validation = passwordConfirmationError(newPassword, confirmPassword);
    if (validation) {
      void adminFeedback(validation, "error");
      return;
    }
    const data = { password: password.value, newPassword: newPassword.value };
    password.value = "";
    busy = form.inert = true;
    try {
      const response = await fetch("/api/admin/password", {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data)
      });
      if (response.ok || response.status === 401) {
        form.reset();
        if (response.ok) await adminFeedback("密码已修改，请用新密码登录。");
        location.replace("/");
        return;
      }
      const result = await response.json();
      void adminFeedback(result.error || "暂时无法修改密码，请重试。", "error");
    } catch {
      void adminFeedback("未收到操作结果。请核对登录状态后再试。", "error");
    } finally {
      busy = form.inert = false;
    }
  });
}

const accessDialog = adminElement("admin-access-dialog");
const accessForm = adminElement("admin-access-form");
const accessClose = adminElement("admin-access-close");
const accessControls = adminElement("admin-access-controls");
const accessStatus = adminElement("admin-access-status");
const accessScope = accessForm.elements.scope;
const accessPassword = accessForm.elements.password;
const accessNewPassword = accessForm.elements.newPassword;
const accessConfirmPassword = accessForm.elements.confirmPassword;
const accessLabels = {
  sessions: "撤销网页登录会话", app_passwords: "撤销应用密码", suspend: "停用账号登录", restore: "恢复账号登录",
  password_reset: "重置主邮箱密码", password_change: "修改自己的密码", admin_grant: "设为全站管理员", admin_revoke: "撤销全站管理员权限", outbound_limit: "更新外发额度", account_create: "创建邮箱账号", storage_alert: "更新容量预警", storage_quota: "更新邮箱硬配额",
  mailbox_retire: "回收邮箱", mailbox_restore: "恢复已回收邮箱", mailbox_purge: "清除邮箱数据", mailbox_rename: "迁移邮箱地址"
};
let accessTarget = "";
let accessRequest = 0;
let accessBusy = false;

function updateAccessImpact() {
  const messages = {
    sessions: accessTarget === signedInAdmin ? "此操作也会退出你当前的网页登录。" : "该账号的网页设备需重新登录。",
    app_passwords: "已撤销的应用密码会在下次连接认证时失效。",
    suspend: "撤销网页会话和应用密码，并阻止新的登录。邮箱内容和收信继续保留。",
    restore: "允许使用主邮箱密码重新登录。恢复后需要重新生成应用密码。",
    password_reset: "新密码须为 12 至 72 字节。重置会撤销旧网页会话和应用密码，已停用账号需单独恢复登录。",
    admin_grant: "该账号将能管理全站域名、邮箱和投递。现有网页登录会话会退出。",
    admin_revoke: "撤销全站管理权限，现有网页登录会话会退出。邮箱和邮件保留。"
  };
  const resetting = accessScope.value === "password_reset";
  adminElement("admin-access-new-password").hidden = !resetting;
  for (const field of [accessNewPassword, accessConfirmPassword]) {
    field.disabled = !resetting;
    field.required = resetting;
    if (!resetting) field.value = "";
  }
  adminElement("admin-access-impact").textContent = messages[accessScope.value] || "";
  const submit = adminElement("admin-access-revoke");
  submit.textContent = "确认" + accessLabels[accessScope.value];
  submit.dataset.intent = ["sessions", "app_passwords", "suspend", "admin_revoke", "password_reset"].includes(accessScope.value) ? "caution" : "normal";
}

function openAccountAccess(email) {
  accessTarget = email;
  accessForm.reset();
  accessControls.hidden = true;
  adminElement("admin-access-counts").hidden = true;
  adminElement("admin-access-history").hidden = true;
  adminElement("admin-access-history").open = false;
  adminElement("admin-access-target").textContent = email;
  adminElement("admin-access-all").href = "/admin/audit?" + new URLSearchParams({ target: email });
  updateAccessImpact();
  accessDialog.showModal();
  void loadAccountAccess("正在读取账号访问状态…");
}

async function loadAccountAccess(message) {
  const revision = ++accessRequest;
  accessStatus.textContent = message;
  try {
    const response = await fetch("/api/admin/accounts/" + encodeURIComponent(accessTarget) + "/access");
    if (revision !== accessRequest) return;
    if (response.status === 401 || response.status === 403) {
      location.replace("/");
      return;
    }
    if (!response.ok) throw new Error("无法读取账号访问状态。");
    const data = await response.json();
    if (revision !== accessRequest) return;
    const account = accounts.find(item => item.email === accessTarget);
    if (account && account.suspended !== data.suspended) {
      account.suspended = data.suspended;
      renderAccounts();
    }
    renderDetails("admin-access-counts", [
      ["账号状态", data.suspended ? "已停用" : "启用"], ["有效网页会话", data.sessions], ["有效应用密码", data.appPasswords]
    ]);
    const selected = accessScope.value;
    const scopes = data.suspended ? ["restore"] : ["sessions", "app_passwords"];
    if (!data.suspended && data.canSuspend) scopes.push("suspend");
    if (data.canSuspend) scopes.push("password_reset");
    if (data.canManageRole && (data.administrator || !data.suspended)) scopes.push(data.administrator ? "admin_revoke" : "admin_grant");
    accessScope.replaceChildren(...scopes.map(scope => new Option(accessLabels[scope], scope)));
    if (scopes.includes(selected)) accessScope.value = selected;
    updateAccessImpact();
    const audit = data.audit.map(record => {
      const item = document.createElement("li");
      const affected = ["sessions", "app_passwords", "suspend", "password_reset", "password_change", "admin_grant", "admin_revoke"].includes(record.action) ? "（撤销 " + record.affected + " 条记录）" : "";
      item.textContent = queueTime(record.stamp) + " · " + record.actor + " · " + accessLabels[record.action] + affected;
      return item;
    });
    adminElement("admin-access-audit").replaceChildren(...audit);
    adminElement("admin-access-empty").hidden = audit.length !== 0;
    adminElement("admin-access-counts").hidden = false;
    adminElement("admin-access-history").hidden = false;
    accessControls.hidden = false;
    if (message.startsWith("正在")) accessStatus.textContent = "";
  } catch {
    if (revision === accessRequest) accessStatus.textContent = "无法读取账号访问状态，请重新打开。";
  }
}

accessScope.addEventListener("change", updateAccessImpact);
accessClose.addEventListener("click", () => accessDialog.close());
accessDialog.addEventListener("cancel", event => {
  if (accessBusy) event.preventDefault();
});
accessDialog.addEventListener("close", () => {
  ++accessRequest;
  accessForm.reset();
});
accessForm.addEventListener("submit", async event => {
  event.preventDefault();
  if (accessBusy || accessControls.hidden) return;
  const data = { scope: accessScope.value, password: accessPassword.value };
  const resetting = data.scope === "password_reset";
  const changingRole = data.scope === "admin_grant" || data.scope === "admin_revoke";
  if (changingRole) data.admin = data.scope === "admin_grant";
  if (resetting) {
    const validation = passwordConfirmationError(accessNewPassword, accessConfirmPassword);
    if (validation) {
      void adminFeedback(validation, "error");
      return;
    }
    data.newPassword = accessNewPassword.value;
    accessNewPassword.value = accessConfirmPassword.value = "";
  }
  accessPassword.value = "";
  accessBusy = accessClose.disabled = accessControls.disabled = true;
  accessStatus.textContent = "正在执行操作…";
  try {
    const response = await fetch("/api/admin/accounts/" + encodeURIComponent(accessTarget) + (changingRole ? "/role" : resetting ? "/password" : "/access"), {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data)
    });
    if (response.status === 401) {
      location.replace("/");
      return;
    }
    const result = await response.json();
    if (!response.ok) {
      if (response.status === 409) await loadAccountAccess("正在读取账号访问状态…");
      else accessStatus.textContent = "";
      void adminFeedback(result.error || "无法更改账号访问状态，请重试。", "error");
      return;
    }
    if (result.signedOut) {
      await adminFeedback("操作已完成，请重新登录。");
      location.replace("/");
      return;
    }
    if (changingRole) void loadOverview();
    const message = changingRole ? (result.admin ? "已设为全站管理员。" : "已撤销全站管理员权限。") : data.scope === "restore" ? "已恢复账号登录。" :
      (resetting ? "已重置密码，撤销 " : data.scope === "suspend" ? "已停用账号登录，撤销 " : "已撤销 ") +
      result.affected + " 条记录。";
    await loadAccountAccess("正在读取账号访问状态…");
    void adminFeedback(message);
  } catch {
    await loadAccountAccess("正在读取账号访问状态…");
    void adminFeedback("未收到操作结果，请查看最近的账号操作记录。", "error");
  } finally {
    accessBusy = accessClose.disabled = accessControls.disabled = false;
  }
});

const deliveryForm = adminElement("admin-queue-filter");
const deliveryItems = adminElement("admin-delivery-items");
const deliveryMore = adminElement("admin-queue-more");
const retryStatus = adminElement("admin-retry-status");
const deliveryDate = new Intl.DateTimeFormat("zh-CN", { dateStyle: "medium", timeStyle: "short" });
let deliveryFilters = new URLSearchParams();
let deliveryNext = "";
let deliveryBusy = false;

function setDeliveryBusy(value) {
  deliveryBusy = value;
  refresh.disabled = deliveryMore.disabled = deliveryForm.querySelector("button").disabled = value;
}

function queueTime(stamp) {
  return stamp ? deliveryDate.format(stamp * 1000) : "—";
}

function deliveryRow(values) {
  const row = document.createElement("tr");
  for (const value of values) {
    const cell = row.insertCell();
    if (value instanceof HTMLElement) cell.append(value);
    else cell.textContent = value;
  }
  return row;
}

function deliveryJobRow(item) {
  let action = "—";
  if (item.canRetry) {
    action = document.createElement("button");
    action.type = "button";
    action.className = "secondary";
    action.textContent = "提前重试";
    action.setAttribute("aria-label", "提前重试任务 " + item.id);
    action.addEventListener("click", () => void retryDelivery(item));
  } else if (item.state === "uncertain" && item.attempts > 0 && !item.tracked) {
    action = document.createElement("button");
    action.type = "button";
    action.className = "secondary";
    action.textContent = "确认结果";
    action.setAttribute("aria-label", "确认任务 " + item.id + " 的结果");
    action.addEventListener("click", () => openDeliveryResolution(item));
  }
  const next = ["queued", "scheduled", "undo"].includes(item.state) ? queueTime(item.due) : "—";
  const state = item.state === "undo" && item.due * 1000 <= Date.now()
    ? "撤销期已结束" : (queueLabels[item.state] || item.state);
  return deliveryRow([
    "#" + item.id + "\n" + item.sender, state + "\n" + item.attempts + " 次",
    queueTime(item.stamp) + "\n" + next, item.reason || "—", action
  ]);
}

async function loadDeliveryQueue(before = "") {
  if (deliveryBusy) return;
  setDeliveryBusy(true);
  statusOutput.textContent = "正在读取投递队列…";
  try {
    const query = new URLSearchParams(deliveryFilters);
    if (before) query.set("before", before);
    const response = await fetch("/api/admin/queue?" + query);
    if (response.status === 401 || response.status === 403) {
      location.replace("/");
      return;
    }
    const data = await response.json();
    if (!response.ok) throw new Error(data.error || "无法读取投递队列，请刷新重试。");
    adminElement("admin-identity").textContent = data.email;
    if (!before) deliveryItems.replaceChildren();
    deliveryItems.append(...data.items.map(deliveryJobRow));
    adminElement("admin-delivery-empty").hidden = deliveryItems.childElementCount !== 0;
    deliveryNext = data.next;
    deliveryMore.hidden = !deliveryNext;
    adminElement("admin-queue-audit").replaceChildren(...data.retries.map(record => deliveryRow([
      queueTime(record.stamp), record.actor, "#" + record.jobId + "\n" + record.sender, queueTime(record.previousDue)
    ])));
    adminElement("admin-retry-audit-empty").hidden = data.retries.length !== 0;
    activePanel.hidden = false;
    statusOutput.textContent = "";
  } catch (error) {
    statusOutput.textContent = error.message;
  } finally {
    setDeliveryBusy(false);
  }
}

async function retryDelivery(item) {
  if (deliveryBusy || !await adminFeedback("将任务 #" + item.id + " 的重试时间提前到现在？", "confirm", "立即重试")) return;
  setDeliveryBusy(true);
  retryStatus.textContent = "正在安排重试…";
  let refreshQueue = false;
  try {
    const response = await fetch("/api/admin/queue/" + item.id + "/retry", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ due: item.due, attempts: item.attempts })
    });
    if (response.status === 401 || response.status === 403) {
      location.replace("/");
      return;
    }
    if (response.status === 204) {
      void adminFeedback("已提前重试时间，等待投递进程处理。");
      refreshQueue = true;
    } else {
      const data = await response.json();
      void adminFeedback(data.error || "安排重试失败，请刷新重试。", "error");
      refreshQueue = response.status === 409;
    }
  } catch {
    void adminFeedback("未收到重试结果，请刷新队列核对。", "error");
  } finally {
    retryStatus.textContent = "";
    setDeliveryBusy(false);
  }
  if (refreshQueue) await loadDeliveryQueue();
}

function setupDeliveryQueue() {
  setupDeliveryResolution();
  const state = adminElement("admin-queue-state");
  state.replaceChildren(new Option("全部状态", ""), ...Object.entries(queueLabels)
    .map(([value, label]) => new Option(label, value)));
  const initial = new URLSearchParams(location.search);
  const initialState = initial.get("state");
  if (Object.hasOwn(queueLabels, initialState)) {
    state.value = initialState;
    deliveryFilters.set("state", initialState);
  }
  const initialID = initial.get("id");
  if (initialID && /^[0-9]{1,19}$/.test(initialID)) {
    deliveryForm.elements.id.value = initialID;
    deliveryFilters.set("id", initialID);
  }
  const initialSender = initial.get("sender");
  if (initialSender && initialSender.length <= 254) {
    deliveryForm.elements.sender.value = initialSender;
    deliveryFilters.set("sender", initialSender);
  }
  deliveryForm.addEventListener("submit", event => {
    event.preventDefault();
    if (deliveryBusy) return;
    deliveryFilters = readAdminFilters(deliveryForm);
    deliveryNext = "";
    deliveryMore.hidden = true;
    deliveryItems.replaceChildren();
    adminElement("admin-delivery-empty").hidden = true;
    retryStatus.textContent = "";
    void loadDeliveryQueue();
  });
  deliveryMore.addEventListener("click", () => {
    if (deliveryNext) void loadDeliveryQueue(deliveryNext);
  });
}

let deliveryResolutionTarget;

function openDeliveryResolution(item) {
  if (deliveryBusy) return;
  deliveryResolutionTarget = item;
  adminElement("admin-delivery-resolution-form").reset();
  adminElement("admin-delivery-resolution-target").textContent = "任务 #" + item.id + " · " + item.sender;
  adminElement("admin-delivery-resolution").showModal();
}

function setupDeliveryResolution() {
  const dialog = adminElement("admin-delivery-resolution");
  const form = adminElement("admin-delivery-resolution-form");
  const close = adminElement("admin-delivery-resolution-close");
  const save = form.querySelector('[type="submit"]');
  close.addEventListener("click", () => dialog.close());
  dialog.addEventListener("close", () => form.reset());
  dialog.addEventListener("cancel", event => { if (save.disabled) event.preventDefault(); });
  window.addEventListener("pageshow", event => { if (event.persisted && dialog.open) dialog.close(); });
  form.addEventListener("submit", async event => {
    event.preventDefault();
    if (save.disabled) return;
    const { id, attempts } = deliveryResolutionTarget;
    const { outcome, password } = form.elements;
    const payload = { attempts, outcome: outcome.value, password: password.value };
    password.value = "";
    close.disabled = save.disabled = true;
    try {
      const response = await fetch("/api/admin/queue/" + id + "/resolve", {
        method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(payload)
      });
      if (response.status === 401) {
        location.replace("/");
        return;
      }
      if (!response.ok) {
        const error = await response.json().catch(() => null);
        void adminFeedback(error?.error || "无法保存投递结果，请稍后重试。", "error");
        return;
      }
      dialog.close();
      await loadDeliveryQueue();
      void adminFeedback("已记录任务 #" + id + " 的核验结果。");
    } catch {
      void adminFeedback("未收到保存结果，请刷新队列核对。", "error");
    } finally {
      close.disabled = save.disabled = false;
    }
  });
}

function readAdminFilters(form) {
  const filters = new URLSearchParams();
  for (const [key, value] of new FormData(form)) {
    if (!value.trim()) continue;
    const entry = key === "since" || key === "until"
      ? Math.floor(new Date(value + "T00:00:00").getTime() / 1000)
      : value.trim();
    filters.set(key, entry);
  }
  return filters;
}

function describeDNS(check) {
  return {
    present: "已发现记录", multiple: "发现多条记录（" + check.count + "）",
    missing: "未发现记录", null: "Null MX（此域声明不接收邮件）",
    skipped: "未填写选择器", error: "查询失败或超时"
  }[check.state];
}

function readinessDNS(name, check, next) {
  return [name, [describeDNS(check), check.state !== "present" ? next : ""].filter(Boolean).join(" · ")];
}

dnsDomain.addEventListener("change", () => {
  dnsResults.hidden = true;
  adminElement("admin-domain-verify").hidden = true;
  adminElement("admin-domain-verify-status").textContent = "";
  dnsStatus.textContent = "";
});

adminElement("admin-dns-form").addEventListener("submit", async event => {
  event.preventDefault();
  if (dnsDomain.disabled || !dnsDomain.value) return;
  const domain = dnsDomain.value;
  const selector = dnsSelector.value.trim();
  dnsDomain.disabled = dnsSelector.disabled = dnsCheck.disabled = true;
  dnsResults.hidden = true;
  dnsStatus.textContent = "正在检查 " + domain + " …";
  try {
    const endpoint = "/api/admin/domains/" + encodeURIComponent(domain) + "/dns?" + new URLSearchParams({ selector, readiness: "1" });
    const response = await fetch(endpoint);
    if (response.status === 401 || response.status === 403) {
      location.replace("/");
      return;
    }
    const data = await response.json();
    if (!response.ok) throw new Error(data.error || "DNS 检查失败，请重试。");
    const mx = data.mx.state === "present"
      ? data.hosts.map(({ Host, Pref }) => Pref + " " + Host.replace(/\.$/, "")).join("、")
      : describeDNS(data.mx);
    const results = [data.mx.state === "present" ? ["MX", mx] : readinessDNS("MX", data.mx, "核对收信主机的 MX"),
      readinessDNS("SPF", data.spf, "核对发信公网 IP 或中继的 SPF 授权"),
      readinessDNS("DMARC", data.dmarc, "核对 _dmarc TXT"),
      readinessDNS("DKIM", data.dkim, "核对选择器和 DNS 公钥")];
    if (data.key) {
      results.push(["内置 DKIM", data.key.keyState === "match" && data.key.dnsState === "match"
        ? "私钥与 DNS 公钥匹配" : "核对私钥文件和选择器 " + data.key.selector]);
    }
    if (data.tls) results.push(["公网 HTTPS", data.tls.state === "pass"
      ? "证书有效至 " + new Date(data.tls.until * 1000).toLocaleDateString("zh-CN") : data.tls.next]);
    if (data.relay) results.push([data.outboundMode === "relay" ? "SMTP 中继" : "外发投递", data.relay.state === "pass" ? "连接、TLS 及已配置认证通过" : data.relay.next]);
    renderDetails("admin-dns-results", results);
    adminElement("admin-domain-verify").hidden = !data.tls;
    dnsResults.hidden = false;
    dnsStatus.textContent = "检查结果 · " + domain + (data.dkimSelector ? " · DKIM " + data.dkimSelector : "");
  } catch (error) {
    dnsStatus.textContent = error.message;
  } finally {
    dnsDomain.disabled = dnsSelector.disabled = dnsCheck.disabled = false;
  }
});

adminElement("admin-domain-verify").addEventListener("submit", async event => {
  event.preventDefault();
  const form = event.currentTarget;
  const file = form.elements.namedItem("message").files[0];
  const status = adminElement("admin-domain-verify-status");
  const button = form.querySelector("button");
  if (!file || !dnsDomain.value) return;
  if (file.size > 32 * 1024 * 1024) {
    status.textContent = "请选择 32 MiB 以内的 EML 原件。";
    return;
  }
  button.disabled = true;
  status.textContent = "正在验证签名…";
  try {
    const domain = dnsDomain.value;
    const query = new URLSearchParams({ selector: dnsSelector.value.trim() });
    const response = await fetch("/api/admin/domains/" + encodeURIComponent(domain) + "/verify?" + query,
      { method: "POST", headers: { "Content-Type": "message/rfc822" }, body: file });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error);
    const messages = { pass: "签名验证通过", fail: "签名验证失败，请核对中继是否改写正文", no_signature: "未找到该域的签名",
      temporary_error: "DNS 暂时不可用，请稍后重试", invalid_message: "无法读取邮件原件", too_many_signatures: "邮件签名过多" };
    status.textContent = domain + " · " + (messages[result.status] || "无法确认签名");
  } catch (error) {
    status.textContent = error.message || "无法验证实信。";
  } finally {
    button.disabled = false;
  }
});

accountDomain.addEventListener("change", renderAccounts);
accountRole.addEventListener("change", renderAccounts);
accountStatus.addEventListener("change", renderAccounts);
find.addEventListener("input", renderAccounts);
if (showAccounts) {
  adminElement("admin-account-sheet-close").addEventListener("click", () => accountSheet.close());
  accountSheet.addEventListener("close", () => {
    const trigger = accountList.querySelector("[data-email=" + CSS.escape(selectedAccountEmail) + "]");
    (trigger || find).focus({ preventScroll: true });
  });
  accountSheetAccess.addEventListener("click", () => openAccountAccess(selectedAccountEmail));
  accountSheetLifecycle.addEventListener("click", () => {
    const account = accounts.find(item => item.email === selectedAccountEmail);
    if (account?.configuredAdmin && account.admin && account.email !== signedInAdmin) {
      void adminFeedback("请先用该安装管理员账号登录，在自己的邮箱详情中选择「交接并回收」。交接完成后，可在此永久删除。", "info");
    } else if (account) openMailboxLifecycle(account);
  });
}
const loadPage = showDelivery ? setupAdminDeliveryTools() : showDMARC ? setupAdminDMARC() : showQuarantine ? setupAdminQuarantine() : showStorage ? setupAdminStorage() : showSecurity ? setupAdminSecurity() : showRuntime ? setupAdminRuntime() : showAliases ? setupAdminAliases() :
  showAudit ? setupAdminAudit() : showQueue ? () => loadDeliveryQueue() : loadOverview;
refresh.addEventListener("click", loadPage);
logout.addEventListener("click", async () => {
  logout.disabled = true;
  try {
    const response = await fetch("/api/logout", { method: "POST" });
    if (!response.ok && response.status !== 401) throw new Error("logout failed");
    location.replace("/");
  } catch {
    statusOutput.textContent = "退出失败，请重试。";
    logout.disabled = false;
  }
});
window.addEventListener("pageshow", event => {
  if (event.persisted) {
    if (accessDialog.open) accessDialog.close();
    if (accountSheet.open) accountSheet.close();
    adminMenu.close();
    void refreshAdminTheme();
    activePanel.hidden = true;
    void loadPage();
  }
});
if (showQueue) setupDeliveryQueue();
if (showDelivery || showDMARC) {
  void fetch("/api/session").then(response => response.ok ? response.json() : null).then(session => {
    if (session?.email) adminElement("admin-identity").textContent = session.email;
  }).catch(() => {});
}
const openAccountCreate = showAccounts ? setupAdminAccountCreate() : null;
const openDNSPlan = showDomains ? setupDNSPlan() : null;
const openDomainEditor = showDomains ? setupAdminDomains() : null;
const openDKIMEditor = showDomains && !domainAdminOnly ? setupAdminDKIM() : null;
if (showAccounts && !domainAdminOnly) setupMailboxLifecycle();
void refreshAdminTheme();
void loadPage();
