import assert from "node:assert/strict";
import { spawn, execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { createConnection } from "node:net";
import { createInterface } from "node:readline";
import { once } from "node:events";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { chromium } from "playwright";

const dir = await mkdtemp(join(tmpdir(), "harbor-browser-"));
const binary = join(dir, process.platform === "win32" ? "harbormail.exe" : "harbormail");
const sender = "sender@harbor.test";
const recipient = "recipient@harbor.test";
const password = "synthetic-browser-password";
const recovery = "346f3255655c4e70aade1f602ac08b48";
let child, closed, browser, smtpAddress, logs = "";

async function login(page, address, base, mfa = false) {
  await page.goto(base);
  await page.locator('#login-form [name="email"]').fill(address);
  await page.locator('#login-form [name="password"]').fill(password);
  await page.locator('#login-form button[type="submit"]').click();
  if (mfa) {
    await page.locator("#login-mfa-field").waitFor({ state: "visible" });
    await page.locator('#login-form [name="code"]').fill("bad-code");
    const rejected = page.waitForResponse(response => response.url().endsWith("/api/login"));
    await page.locator('#login-form button[type="submit"]').click();
    assert.equal((await rejected).status(), 428);
    await page.locator('#login-form [name="code"]').fill(recovery);
    await page.locator('#login-form button[type="submit"]').click();
  }
  await page.locator("#compose").waitFor();
}

async function injectHTML(address) {
  const port = Number(address.split(":").at(-1));
  const socket = createConnection({ host: "127.0.0.1", port });
  socket.setTimeout(5000, () => socket.destroy(new Error("SMTP fixture timed out")));
  const lines = createInterface({ input: socket })[Symbol.asyncIterator]();
  async function exchange(command, expected) {
    if (command) socket.write(command + "\r\n");
    for (;;) {
      const { value, done } = await lines.next();
      assert.equal(done, false);
      assert.ok(value.startsWith(expected), value);
      if (value[3] !== "-") return;
    }
  }
  try {
    await exchange(null, "220");
    await exchange("EHLO local.harbor.test", "250");
    await exchange("MAIL FROM:<fixture@harbor.test>", "250");
    await exchange("RCPT TO:<" + recipient + ">", "250");
    await exchange("DATA", "354");
    await exchange([
      "From: fixture@harbor.test", "To: " + recipient, "Subject: HTML regression",
      "MIME-Version: 1.0", "Content-Type: text/html; charset=utf-8", "",
      "<h1>HTML fixture body</h1><script>activeMarker</script><img src=\"https://tracker.invalid/pixel\">", ".",
    ].join("\r\n"), "250");
  } finally {
    socket.destroy();
  }
}

try {
  execFileSync("go", ["build", "-buildvcs=false", "-o", binary, "./cmd/harbormail"], { stdio: "inherit" });
  const hash = execFileSync(binary, ["hash-password"], { input: password + "\n", encoding: "utf8" }).trim();
  const env = Object.fromEntries(Object.entries(process.env).filter(([name]) => !name.toUpperCase().startsWith("HARBOR_")));
  Object.assign(env, {
    HARBOR_DOMAIN: "harbor.test",
    HARBOR_ACCOUNTS: JSON.stringify({ [sender]: hash, [recipient]: hash }),
    HARBOR_DB: join(dir, "mail.db"),
    HARBOR_ADMINS: sender,
    HARBOR_ADMIN_MFA: JSON.stringify({
      [sender]: {
        secret: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
        recoveryHashes: [createHash("sha256").update("Harbor Mail admin recovery\0" + recovery).digest("hex")],
      },
    }),
    HARBOR_DIRECT_MX: "false",
    HARBOR_WEB: "127.0.0.1:0",
    HARBOR_SMTP: "127.0.0.1:0",
  });
  child = spawn(binary, [], { env, cwd: dir, stdio: ["ignore", "ignore", "pipe"] });
  closed = once(child, "close");
  const base = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("Webmail did not start within 20 seconds")), 20000);
    child.once("close", code => {
      clearTimeout(timer);
      reject(new Error("Webmail exited with code " + code));
    });
    child.once("error", error => {
      clearTimeout(timer);
      reject(error);
    });
    child.stderr.setEncoding("utf8").on("data", chunk => {
      logs += chunk;
      const address = logs.match(/Webmail listening on (127\.0\.0\.1:\d+)/);
      smtpAddress = logs.match(/SMTP listening on (127\.0\.0\.1:\d+)/)?.[1];
      if (address && smtpAddress) {
        clearTimeout(timer);
        resolve("http://" + address[1]);
      }
    });
  });

  browser = await chromium.launch();
  const author = await browser.newPage();
  author.setDefaultTimeout(10000);
  await login(author, sender, base, true);
  await author.goto(base + "/admin");
  await author.locator("#admin-overview").waitFor();
  assert.equal((await author.request.get(base + "/api/admin/accounts")).status(), 200);
  await author.goto(base);
  await author.locator("#compose").waitFor();
  await author.locator("#compose").click();
  await author.locator("#compose-recipient").fill(recipient);
  await author.locator('#editor-form [name="subject"]').fill("Browser regression");
  await author.locator("#compose-body").fill("Synthetic message body");
  await author.locator("#draft-save-message").filter({ hasText: "草稿已保存" }).waitFor();
  await author.reload();
  await author.locator('[data-folder="drafts"]').click();
  await author.locator("#messages").getByText("Browser regression", { exact: true }).click();
  await author.locator("#edit-draft").click();
  await author.locator("#editor").waitFor();
  assert.equal(await author.locator("#compose-body").inputValue(), "Synthetic message body");
  assert.equal(await author.locator("#compose-recipient").inputValue(), recipient);

  await author.locator("#toggle-schedule").click();
  await author.locator('#editor-form button[type="submit"]').click();
  await author.locator('[data-folder="outbox"]').click();
  await author.locator("#messages").getByText("Browser regression", { exact: true }).click();
  await author.locator("#delivery-status").filter({ hasText: "等待定时发送" }).waitFor();
  await author.locator("#cancel-scheduled").click();
  await author.locator("#edit-draft").click();
  await author.locator('#editor-form button[type="submit"]').click();
  await author.locator('[data-folder="sent"]').click();
  await author.locator("#messages").getByText("Browser regression", { exact: true }).waitFor();

  const reader = await browser.newPage();
  reader.setDefaultTimeout(10000);
  await login(reader, recipient, base);
  await reader.locator("#messages").getByText("Browser regression", { exact: true }).click();
  await reader.getByText("Synthetic message body", { exact: true }).waitFor();
  assert.equal((await reader.request.get(base + "/api/admin/accounts")).status(), 403);
  await reader.goto(base + "/admin");
  await reader.waitForURL(url => url.searchParams.get("notice") === "admin-forbidden");
  await injectHTML(smtpAddress);
  await reader.reload();
  await reader.locator("#messages").getByText("HTML regression", { exact: true }).click();
  const frame = reader.frameLocator("#html-frame");
  await frame.getByRole("heading", { name: "HTML fixture body" }).waitFor();
  assert.equal(await frame.locator("script").count(), 0);
  assert.equal(await frame.locator('img[src^="https:"]').count(), 0);
  console.log("Browser regression passed: MFA, admin access, permission denial, drafts, local delivery and HTML sanitization");
} catch (error) {
  if (logs) console.error(logs);
  throw error;
} finally {
  try {
    await browser?.close();
  } finally {
    child?.kill();
    await closed?.catch(() => {});
    await rm(dir, { recursive: true, force: true });
  }
}
