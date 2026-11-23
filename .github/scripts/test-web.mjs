import assert from "node:assert/strict";
import { spawn, execFileSync } from "node:child_process";
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
let child, closed, browser, logs = "";

async function login(page, address, base) {
  await page.goto(base);
  await page.locator('#login-form [name="email"]').fill(address);
  await page.locator('#login-form [name="password"]').fill(password);
  await page.locator('#login-form button[type="submit"]').click();
  await page.locator("#compose").waitFor();
}

try {
  execFileSync("go", ["build", "-buildvcs=false", "-o", binary, "./cmd/harbormail"], { stdio: "inherit" });
  const hash = execFileSync(binary, ["hash-password"], { input: password + "\n", encoding: "utf8" }).trim();
  const env = Object.fromEntries(Object.entries(process.env).filter(([name]) => !name.toUpperCase().startsWith("HARBOR_")));
  Object.assign(env, {
    HARBOR_DOMAIN: "harbor.test",
    HARBOR_ACCOUNTS: JSON.stringify({ [sender]: hash, [recipient]: hash }),
    HARBOR_DB: join(dir, "mail.db"),
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
      if (address) {
        clearTimeout(timer);
        resolve("http://" + address[1]);
      }
    });
  });

  browser = await chromium.launch();
  const author = await browser.newPage();
  author.setDefaultTimeout(10000);
  await login(author, sender, base);
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
  console.log("Browser regression passed: login, draft reload, scheduled status, cancellation and local delivery");
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
