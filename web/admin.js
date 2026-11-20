"use strict";

const adminElement = id => document.getElementById(id);
const domainAdminOnly = document.body.hasAttribute("data-domain-admin");
if (domainAdminOnly) {
  for (const page of ["overview", "queue", "runtime", "storage", "security", "quarantine", "audit"]) {
    adminElement("admin-" + page + "-link").hidden = true;
  }
  adminElement("admin-domain-add").hidden = true;
}
const domainFromLink = new URLSearchParams(location.search).get("domain");
const showDomains = location.pathname === "/admin/domains";
const showQueue = location.pathname === "/admin/queue";
const showAudit = location.pathname === "/admin/audit";
const showAliases = location.pathname === "/admin/aliases";
const showRuntime = location.pathname === "/admin/runtime";
const showStorage = location.pathname === "/admin/storage";
const showQuarantine = location.pathname === "/admin/quarantine";
const showSecurity = location.pathname === "/admin/security";
const showAccounts = location.pathname === "/admin/accounts" ||
  ((location.pathname === "/admin" || location.pathname === "/admin/") && Boolean(domainFromLink));
const section = showQuarantine ? "quarantine" : showStorage ? "storage" : showSecurity ? "security" : showRuntime ? "runtime" : showAliases ? "aliases" : showAudit ? "audit" : showQueue ? "queue" :
  showDomains ? "domains" : showAccounts ? "accounts" : "overview";
const activePanel = adminElement({
  audit: "admin-audit-view", queue: "admin-delivery", domains: "admin-domains", accounts: "admin-account-view",
  aliases: "admin-alias-view", runtime: "admin-runtime-view", storage: "admin-storage-view",
  quarantine: "admin-quarantine-view", security: "admin-security-view", overview: "admin-overview"
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
adminElement("admin-" + section + "-link").setAttribute("aria-current", "page");
if (section !== "overview") {
  const title = { queue: "投递队列", domains: "域名", accounts: "邮箱账号", audit: "操作审计",
    aliases: "别名管理", runtime: "运行监控", storage: "存储用量", security: "安全", quarantine: "隔离邮件" }[section];
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

function renderAccounts() {
  const query = find.value.trim().toLowerCase();
  const matching = accounts.filter(account => {
    const addresses = [account.email, ...(account.aliases || [])];
    const role = account.admin ? "admin" : "user";
    const status = account.retired ? "retired" : account.suspended ? "suspended" : "active";
    return (!accountRole.value || accountRole.value === role) &&
      (!accountStatus.value || accountStatus.value === status) &&
      addresses.some(address => !accountDomain.value || domainOf(address) === accountDomain.value) &&
      addresses.some(address => address.toLowerCase().includes(query));
  });
  const content = document.createDocumentFragment();
  for (const account of matching) {
    const row = document.createElement("tr");
    for (const value of [account.email, account.admin ? "管理员" : "普通用户",
      account.retired ? "已回收" : account.suspended ? "已停用" : "启用",
      account.managed ? "后台" : "服务器配置", account.aliases?.join("、") || "无"]) {
      row.insertCell().textContent = value;
    }
    const button = document.createElement("button");
    button.type = "button";
    button.className = "secondary";
    button.textContent = "管理访问";
    button.setAttribute("aria-label", "管理 " + account.email + " 的访问");
    button.addEventListener("click", () => openAccountAccess(account.email));
    const actions = row.insertCell();
    actions.className = "admin-account-actions";
    if (domainAdminOnly) {
      actions.textContent = "—";
    } else {
      if (!account.retired) actions.append(button);
      actions.append(overviewLink("查看用量", "/admin/storage?" + new URLSearchParams({ owner: account.email })));
      if (account.managed) {
        const lifecycle = document.createElement("button");
        lifecycle.type = "button";
        lifecycle.className = "secondary";
        lifecycle.textContent = "邮箱管理";
        lifecycle.addEventListener("click", () => openMailboxLifecycle(account));
        actions.append(lifecycle);
      }
    }
    content.append(row);
  }
  adminElement("admin-accounts").replaceChildren(content);
  adminElement("admin-empty").hidden = matching.length !== 0;
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
  password_reset: "重置主邮箱密码", account_create: "创建邮箱账号", storage_alert: "更新容量预警", storage_quota: "更新邮箱硬配额",
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
    password_reset: "新密码须为 12 至 72 字节。重置会撤销旧网页会话和应用密码，已停用账号需单独恢复登录。"
  };
  const resetting = accessScope.value === "password_reset";
  adminElement("admin-access-new-password").hidden = !resetting;
  for (const field of [accessNewPassword, accessConfirmPassword]) {
    field.disabled = !resetting;
    field.required = resetting;
    if (!resetting) field.value = "";
  }
  adminElement("admin-access-impact").textContent = messages[accessScope.value] || "";
  adminElement("admin-access-revoke").textContent = "确认" + accessLabels[accessScope.value];
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
    accessScope.replaceChildren(...scopes.map(scope => new Option(accessLabels[scope], scope)));
    if (scopes.includes(selected)) accessScope.value = selected;
    updateAccessImpact();
    const audit = data.audit.map(record => {
      const item = document.createElement("li");
      const affected = ["sessions", "app_passwords", "suspend", "password_reset"].includes(record.action) ? "（撤销 " + record.affected + " 条记录）" : "";
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
  if (resetting) {
    if (accessNewPassword.value !== accessConfirmPassword.value) {
      accessStatus.textContent = "两次输入的新密码不一致。";
      accessConfirmPassword.focus();
      return;
    }
    const length = new TextEncoder().encode(accessNewPassword.value).length;
    if (length < 12 || length > 72) {
      accessStatus.textContent = "新密码长度须为 12 至 72 字节。";
      accessNewPassword.focus();
      return;
    }
    data.newPassword = accessNewPassword.value;
    accessNewPassword.value = accessConfirmPassword.value = "";
  }
  accessPassword.value = "";
  accessBusy = accessClose.disabled = accessControls.disabled = true;
  accessStatus.textContent = "正在执行操作…";
  try {
    const response = await fetch("/api/admin/accounts/" + encodeURIComponent(accessTarget) + (resetting ? "/password" : "/access"), {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data)
    });
    if (response.status === 401) {
      location.replace("/");
      return;
    }
    const result = await response.json();
    if (!response.ok) {
      if (response.status === 409) await loadAccountAccess(result.error);
      else accessStatus.textContent = result.error || "无法更改账号访问状态，请重试。";
      return;
    }
    if (result.signedOut) {
      location.replace("/");
      return;
    }
    const message = data.scope === "restore" ? "已恢复账号登录。" :
      (resetting ? "已重置密码，撤销 " : data.scope === "suspend" ? "已停用账号登录，撤销 " : "已撤销 ") +
      result.affected + " 条记录。";
    await loadAccountAccess(message);
  } catch {
    await loadAccountAccess("未收到操作结果，请查看最近的账号操作记录。");
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
  if (deliveryBusy || !confirm("将任务 #" + item.id + " 的重试时间提前到现在？")) return;
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
      retryStatus.textContent = "已提前重试时间，等待投递进程处理。";
      refreshQueue = true;
    } else {
      const data = await response.json();
      retryStatus.textContent = data.error || "安排重试失败，请刷新重试。";
      refreshQueue = response.status === 409;
    }
  } catch {
    retryStatus.textContent = "未收到重试结果，请刷新队列核对。";
  } finally {
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
  adminElement("admin-delivery-resolution-error").textContent = "";
  adminElement("admin-delivery-resolution-target").textContent = "任务 #" + item.id + " · " + item.sender;
  adminElement("admin-delivery-resolution").showModal();
}

function setupDeliveryResolution() {
  const dialog = adminElement("admin-delivery-resolution");
  const form = adminElement("admin-delivery-resolution-form");
  const close = adminElement("admin-delivery-resolution-close");
  const save = form.querySelector('[type="submit"]');
  const output = adminElement("admin-delivery-resolution-error");
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
    output.textContent = "";
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
        output.textContent = error?.error || "无法保存投递结果，请稍后重试。";
        return;
      }
      dialog.close();
      await loadDeliveryQueue();
      retryStatus.textContent = "已记录任务 #" + id + " 的核验结果。";
    } catch {
      output.textContent = "未收到保存结果，请刷新队列核对。";
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
const loadPage = showQuarantine ? setupAdminQuarantine() : showStorage ? setupAdminStorage() : showSecurity ? setupAdminSecurity() : showRuntime ? setupAdminRuntime() : showAliases ? setupAdminAliases() :
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
    activePanel.hidden = true;
    void loadPage();
  }
});
if (showQueue) setupDeliveryQueue();
const openAccountCreate = showAccounts ? setupAdminAccountCreate() : null;
const openDNSPlan = showDomains ? setupDNSPlan() : null;
const openDomainEditor = showDomains ? setupAdminDomains() : null;
const openDKIMEditor = showDomains && !domainAdminOnly ? setupAdminDKIM() : null;
if (showAccounts && !domainAdminOnly) setupMailboxLifecycle();
void loadPage();
