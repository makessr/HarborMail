import assert from "node:assert/strict";
import { spawn, execFileSync } from "node:child_process";
import { createConnection } from "node:net";
import { createInterface } from "node:readline";
import { once } from "node:events";
import { mkdtemp, mkdir, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { chromium } from "playwright";

// Captures the README screenshots from a freshly built binary and synthetic demo data.
//
//   node .github/scripts/capture-screenshots.mjs --out screenshots [--keep]
//   node .github/scripts/capture-screenshots.mjs --out docs/assets --webp
//
// The script builds ./cmd/harbormail into a temporary directory, boots it with SQLite on
// ephemeral ports, delivers six demo messages over SMTP, and writes one PNG per screenshot.
// --webp converts the PNGs in place with Pillow, which is how docs/assets/*.webp are produced.
// Demo data is deterministic apart from timestamps, which follow the capture time.

const args = process.argv.slice(2);
function argument(name, fallback) {
  const index = args.indexOf(name);
  if (index < 0) return fallback;
  const value = args[index + 1];
  assert.ok(value && !value.startsWith("--"), name + " needs a value");
  return value;
}
const outDir = resolve(argument("--out", "screenshots"));
const keep = args.includes("--keep");
const toWebp = args.includes("--webp");
const width = 1440;

const password = "synthetic-screenshot-password";
const domain = "harbor.test";
const admin = "admin@" + domain;
const accounts = [admin, "operations@" + domain, "support@" + domain, "studio@studio.test"];
const baseDate = Date.now();

const demo = [
  {
    from: ["发布通知", "release@example.test"],
    subject: "Harbor Mail v0.2.25 已发布",
    hours: 0.3,
    body: [
      "你好，",
      "",
      "Harbor Mail v0.2.25 已发布。本次更新完善了账号自助、地址回收和自动化运维。",
      "",
      "• 网页邮箱可以修改密码，也可以用一次性重置码找回账号",
      "• 管理后台的账号详情可以重置密码、删除账号",
      "• 已回收的邮箱地址可以重新分配",
      "• 存储用量页按邮箱显示占用",
      "• 全量 Compose 可以用一条命令启用本机备份与巡检",
      "",
      "这是一封用于 README 截图的演示邮件。",
    ],
  },
  {
    from: ["工作台", "workspace@example.test"],
    subject: "欢迎使用团队邮箱",
    hours: 2,
    body: [
      "你好，",
      "",
      "可以在侧栏新建文件夹、管理标签，或在搜索框里用 from:/to:/has:attachment 组合查询。",
      "",
      "归档、稍后处理和草稿箱都已经开启，快捷键可以在设置里查看。",
    ],
  },
  {
    from: ["服务监控", "health@example.test"],
    subject: "备份校验通过",
    hours: 3,
    body: [
      "昨晚的数据库备份已经完成回读校验，邮件、队列和账号计数一致。",
      "",
      "失败时会按配置的巡检间隔重新告警，不需要人工确认。",
    ],
  },
  {
    from: ["项目团队", "team@example.test"],
    subject: "周报提交提醒",
    hours: 4,
    body: ["请在周五前提交本周进展，并注明下周计划。", "", "需要补的字段可以在模板里直接复制。"],
  },
  {
    from: ["运营中心", "ops@example.test"],
    subject: "下周维护安排",
    hours: 26,
    body: [
      "下周三安排例行维护。维护前会备份数据库和 DKIM 密钥，并用同一份备份做隔离回读。",
      "",
      "维护窗口约 20 分钟，期间收发信会短暂暂停。",
    ],
  },
  {
    from: ["支持团队", "support@example.test"],
    subject: "域名配置检查已完成",
    hours: 28,
    body: ["邮件域名配置检查已经完成。下一步可在管理后台的域名页生成 DNS 记录并发布 DKIM。"],
  },
];

function encodeWord(text) {
  return "=?UTF-8?B?" + Buffer.from(text, "utf8").toString("base64") + "?=";
}

function rfc2822(time) {
  const date = new Date(time);
  const zone = -date.getTimezoneOffset();
  const sign = zone < 0 ? "-" : "+";
  const offset = Math.abs(zone);
  const pad = value => String(value).padStart(2, "0");
  return ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"][date.getDay()] + ", "
    + date.getDate() + " " + ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"][date.getMonth()]
    + " " + date.getFullYear() + " " + pad(date.getHours()) + ":" + pad(date.getMinutes()) + ":" + pad(date.getSeconds())
    + " " + sign + pad(Math.floor(offset / 60)) + pad(offset % 60);
}

async function exchange(socket, lines, command, expected) {
  if (command) socket.write(command + "\r\n");
  for (;;) {
    const { value, done } = await lines.next();
    assert.equal(done, false, "SMTP connection closed early");
    assert.ok(value.startsWith(expected), value);
    if (value[3] !== "-") return;
  }
}

async function deliver(address, message) {
  const port = Number(address.split(":").at(-1));
  const socket = createConnection({ host: "127.0.0.1", port });
  socket.setTimeout(5000, () => socket.destroy(new Error("SMTP fixture timed out")));
  const lines = createInterface({ input: socket })[Symbol.asyncIterator]();
  try {
    await exchange(socket, lines, null, "220");
    await exchange(socket, lines, "EHLO local." + domain, "250");
    await exchange(socket, lines, "MAIL FROM:<" + message.from[1] + ">", "250");
    await exchange(socket, lines, "RCPT TO:<" + message.to + ">", "250");
    await exchange(socket, lines, "DATA", "354");
    await exchange(socket, lines, message.raw, "250");
  } finally {
    socket.destroy();
  }
}

function demoMessage(item, recipient) {
  const time = baseDate - item.hours * 3600 * 1000;
  const raw = [
    "From: " + encodeWord(item.from[0]) + " <" + item.from[1] + ">",
    "To: " + recipient,
    "Subject: " + encodeWord(item.subject),
    "Date: " + rfc2822(time),
    "MIME-Version: 1.0",
    "Content-Type: text/plain; charset=utf-8",
    "Content-Transfer-Encoding: 8bit",
    "",
    item.body.join("\r\n"),
    ".",
  ].join("\r\n");
  return { from: item.from, to: recipient, raw };
}

async function login(page, base, address) {
  await page.goto(base);
  await page.locator('#login-form [name="email"]').fill(address);
  await page.locator('#login-form [name="password"]').fill(password);
  await page.locator('#login-form button[type="submit"]').click();
  await page.locator("#compose").waitFor();
}

async function settle(page) {
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(250);
}

async function convertToWebp(dir) {
  const script = [
    "import glob, os, sys",
    "from PIL import Image",
    "for path in sorted(glob.glob(os.path.join(sys.argv[1], '*.png'))):",
    "    Image.open(path).convert('RGB').save(os.path.splitext(path)[0] + '.webp', 'WEBP', quality=82, method=6)",
  ].join("\n");
  for (const python of [process.env.HARBOR_SCREENSHOT_PYTHON, "python3", "python"].filter(Boolean)) {
    try {
      execFileSync(python, ["-c", script, dir], { stdio: "inherit" });
      for (const name of ["webmail", "webmail-compose", "admin-overview", "admin-accounts", "admin-dns", "admin-storage", "admin-runtime"]) {
        await rm(join(dir, name + ".png"), { force: true });
      }
      console.log("converted to WebP");
      return;
    } catch {
      // Try the next interpreter.
    }
  }
  throw new Error("Pillow is required for --webp; set HARBOR_SCREENSHOT_PYTHON to a Python with Pillow installed");
}

if (args.includes("--convert-only")) {
  await convertToWebp(outDir);
  process.exit(0);
}

let child, closed, browser, work, logs = "";

try {
  await mkdir(outDir, { recursive: true });
  work = await mkdtemp(join(tmpdir(), "harbor-screenshots-"));
  const binary = join(work, process.platform === "win32" ? "harbormail.exe" : "harbormail");
  let version = process.env.HARBOR_SCREENSHOT_VERSION || "v0.0.0-dev";
  if (!process.env.HARBOR_SCREENSHOT_VERSION) {
    try {
      version = execFileSync("git", ["-c", "safe.directory=*", "describe", "--tags", "--abbrev=0"], { encoding: "utf8" }).trim();
    } catch {
      // Keep the placeholder when the checkout has no tags.
    }
  }
  console.log("capturing version " + version);
  execFileSync("go", [
    "build", "-buildvcs=false", "-trimpath",
    "-ldflags", "-s -w -X harbormail/internal/server.buildVersion=" + version,
    "-o", binary, "./cmd/harbormail",
  ], { stdio: "inherit" });
  const hash = execFileSync(binary, ["hash-password"], { input: password + "\n", encoding: "utf8" }).trim();

  const env = Object.fromEntries(Object.entries(process.env).filter(([name]) => !name.toUpperCase().startsWith("HARBOR_")));
  Object.assign(env, {
    HARBOR_DOMAIN: domain,
    HARBOR_ACCOUNTS: JSON.stringify(Object.fromEntries(accounts.map(address => [address, hash]))),
    HARBOR_DB: join(work, "mail.db"),
    HARBOR_ADMINS: admin,
    HARBOR_DKIM_KEY: "70".repeat(32),
    HARBOR_DIRECT_MX: "false",
    HARBOR_WEB: "127.0.0.1:0",
    HARBOR_SMTP: "127.0.0.1:0",
  });
  child = spawn(binary, [], { env, cwd: work, stdio: ["ignore", "ignore", "pipe"] });
  closed = once(child, "close");
  let smtpAddress;
  const base = await new Promise((resolveBase, reject) => {
    const timer = setTimeout(() => reject(new Error("webmail did not start within 20 seconds")), 20000);
    child.once("close", code => {
      clearTimeout(timer);
      reject(new Error("webmail exited with code " + code));
    });
    child.once("error", error => {
      clearTimeout(timer);
      reject(error);
    });
    child.stderr.setEncoding("utf8").on("data", chunk => {
      logs += chunk;
      const web = logs.match(/Webmail listening on (127\.0\.0\.1:\d+)/);
      smtpAddress = logs.match(/SMTP listening on (127\.0\.0\.1:\d+)/)?.[1];
      if (web && smtpAddress) {
        clearTimeout(timer);
        resolveBase("http://" + web[1]);
      }
    });
  });

  // Oldest first so the newest message ends up at the top of the inbox.
  for (const item of [...demo].reverse()) await deliver(smtpAddress, demoMessage(item, admin));

  browser = await chromium.launch();
  const page = await browser.newPage({ viewport: { width, height: 830 }, deviceScaleFactor: 1 });
  page.setDefaultTimeout(15000);
  await login(page, base, admin);

  await page.locator('[data-folder="inbox"]').click();
  await page.locator("#messages .row").first().waitFor();
  await page.locator("#messages .row").first().click();
  await page.locator("#subject").filter({ hasText: "Harbor Mail v0.2.25 已发布" }).waitFor();
  await settle(page);
  await page.screenshot({ path: join(outDir, "webmail.png") });
  console.log("captured webmail");

  await page.locator("#compose").click();
  await page.locator("#editor").waitFor();
  await page.locator("#compose-recipient").fill("support@" + domain);
  await page.locator('#editor-form [name="subject"]').fill("周五维护确认");
  await page.locator("#compose-body").fill([
    "你好，",
    "",
    "请确认周五的维护安排和备份窗口。更新完成后我们会验证邮箱登录、SMTP 收发和数据库连接。",
    "",
    "谢谢。",
  ].join("\n"));
  // Wait for the draft autosave to settle, then move focus off the body so no caret shows.
  await page.locator("#draft-save-message").filter({ hasText: "草稿已保存" }).waitFor();
  await page.locator("#editor-title").click();
  await settle(page);
  await page.screenshot({ path: join(outDir, "webmail-compose.png") });
  console.log("captured webmail-compose");

  await page.setViewportSize({ width, height: 900 });
  await page.goto(base + "/admin");
  await page.locator("#admin-overview").waitFor();
  // Collapse the first-run guide so the overview shows the steady-state layout.
  if (await page.locator("#admin-onboarding-hide").isVisible()) {
    await page.locator("#admin-onboarding-hide").click();
    await page.locator("#admin-onboarding").waitFor({ state: "hidden" });
  }
  await settle(page);
  await page.screenshot({ path: join(outDir, "admin-overview.png"), fullPage: true });
  console.log("captured admin-overview");

  await page.setViewportSize({ width, height: 780 });
  await page.goto(base + "/admin/accounts");
  await page.getByText("共 " + accounts.length + " 个账号").waitFor();
  await page.locator(".admin-account-open").first().click();
  await page.locator("#admin-account-sheet").waitFor({ state: "visible" });
  await settle(page);
  await page.screenshot({ path: join(outDir, "admin-accounts.png") });
  console.log("captured admin-accounts");

  await page.setViewportSize({ width, height: 940 });
  await page.goto(base + "/admin/domains");
  await page.getByRole("button", { name: "配置 studio.test 的 DNS 记录" }).waitFor();
  // Generate a pending DKIM key so the DNS plan shows the real TXT record.
  await page.getByRole("button", { name: "管理 studio.test 的 DKIM" }).click();
  await page.locator("#admin-dkim-dialog").waitFor({ state: "visible" });
  if (await page.locator("#admin-dkim-controls").isVisible()) {
    await page.locator('#admin-dkim-form [name="password"]').fill(password);
    await page.getByRole("button", { name: "生成密钥" }).click();
    await page.locator("#admin-dkim-dns").waitFor({ state: "visible" });
  }
  await page.locator("#admin-dkim-close").click();
  await page.getByRole("button", { name: "配置 studio.test 的 DNS 记录" }).click();
  await page.locator("#admin-dns-plan-dialog").waitFor({ state: "visible" });
  await page.locator('#admin-dns-plan-form [name="host"]').fill("mail.studio.test");
  await page.locator('#admin-dns-plan-form [name="ipv4"]').fill("198.51.100.24");
  await page.locator('#admin-dns-plan-form [name="spfInclude"]').fill("_spf.example.test");
  await page.locator("#admin-dns-plan-build").click();
  await page.locator("#admin-dns-plan-output").waitFor({ state: "visible" });
  await page.locator("#admin-dns-plan-records tr").first().waitFor();
  await settle(page);
  await page.screenshot({ path: join(outDir, "admin-dns.png") });
  console.log("captured admin-dns");

  await page.setViewportSize({ width, height: 910 });
  await page.goto(base + "/admin/storage");
  await page.locator("#admin-storage-view").waitFor();
  await settle(page);
  await page.screenshot({ path: join(outDir, "admin-storage.png"), fullPage: true });
  console.log("captured admin-storage");

  await page.setViewportSize({ width, height: 1010 });
  await page.goto(base + "/admin/runtime");
  await page.locator("#admin-runtime-view").waitFor();
  // The runtime page samples every ten seconds; give the charts a few points.
  await page.waitForTimeout(23000);
  await settle(page);
  await page.screenshot({ path: join(outDir, "admin-runtime.png"), fullPage: true });
  console.log("captured admin-runtime");

  if (toWebp) {
    await convertToWebp(outDir);
  } else {
    console.log("PNG files written to " + outDir + "; re-run with --webp to convert them");
  }
} catch (error) {
  if (logs) console.error(logs);
  throw error;
} finally {
  try {
    await browser?.close();
  } finally {
    child?.kill();
    await closed?.catch(() => {});
    if (!keep && work) await rm(work, { recursive: true, force: true }).catch(() => {});
  }
}
