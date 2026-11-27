// Upgrade, rollback and backup/restore drill for a released build.
//
// Steps for one datastore provider:
//   1. build the previous release (from ref) and the current checkout
//   2. seed the old binary with accounts and three messages over SMTP
//   3. back up the datastore while the old binary is stopped
//   4. start the new binary on the old data and verify the messages survived
//   5. restore the backup and start the old binary again (rollback path)
//
// Usage:
//   node .github/scripts/test-upgrade-drill.mjs --from v0.2.24 [--provider sqlite|mysql]
//
// Environment:
//   HARBOR_DRILL_PROVIDER   sqlite (default) or mysql
//   HARBOR_DRILL_FROM       release ref to downgrade to (default: previous tag)
//   HARBOR_DRILL_MYSQL_DSN  user:pass@tcp(host:port)/database?parseTime=true
//   HARBOR_DRILL_PYTHON     python with the sqlite3 module (default: python3)
//   HARBOR_DRILL_KEEP=1     keep the work directory for inspection

import { execFileSync, spawn } from "node:child_process";
import { copyFileSync, existsSync, mkdtempSync, mkdirSync, readFileSync, rmSync } from "node:fs";
import net from "node:net";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const repo = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");
const isWindows = process.platform === "win32";
const binaryName = isWindows ? "harbormail.exe" : "harbormail";
const password = "synthetic-drill-password";
const subjects = ["Drill upgrade message 1", "Drill upgrade message 2", "Drill upgrade message 3"];

function parseArgs(argv) {
  const options = { provider: process.env.HARBOR_DRILL_PROVIDER || "sqlite", from: process.env.HARBOR_DRILL_FROM || "", keep: process.env.HARBOR_DRILL_KEEP === "1" };
  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (arg === "--provider") options.provider = argv[++index];
    else if (arg === "--from") options.from = argv[++index];
    else if (arg === "--keep") options.keep = true;
    else if (arg === "-h" || arg === "--help") {
      console.log("usage: node .github/scripts/test-upgrade-drill.mjs --from REF [--provider sqlite|mysql] [--keep]");
      process.exit(0);
    } else throw new Error(`unknown argument: ${arg}`);
  }
  if (options.provider !== "sqlite" && options.provider !== "mysql") throw new Error("--provider must be sqlite or mysql");
  return options;
}

function git(args, cwd = repo) {
  return execFileSync("git", ["-c", "safe.directory=*", ...args], { cwd, encoding: "utf8" }).trim();
}

function build(cwd, output, version) {
  execFileSync("go", ["build", "-buildvcs=false", "-trimpath", "-ldflags", `-s -w -X harbormail/internal/server.buildVersion=${version}`, "-o", output, "./cmd/harbormail"], { cwd, stdio: "inherit" });
}

function hashPassword(binary) {
  return execFileSync(binary, ["hash-password"], { input: `${password}\n`, encoding: "utf8" }).trim();
}

function startServer(binary, cwd, env) {
  const child = spawn(binary, [], { cwd, env, stdio: ["ignore", "pipe", "pipe"] });
  let output = "";
  const ready = new Promise((accept, reject) => {
    const timer = setTimeout(() => reject(new Error(`server did not become ready in time:\n${output}`)), 30000);
    const scan = (chunk) => {
      output += chunk.toString();
      const web = output.match(/Webmail listening on 127\.0\.0\.1:(\d+)/);
      const smtp = output.match(/SMTP listening on 127\.0\.0\.1:(\d+)/);
      if (web && smtp) {
        clearTimeout(timer);
        accept({ web: Number(web[1]), smtp: Number(smtp[1]), child, logs: () => output });
      }
    };
    child.stdout.on("data", scan);
    child.stderr.on("data", scan);
    child.on("exit", (code) => {
      clearTimeout(timer);
      reject(new Error(`server exited with code ${code}:\n${output}`));
    });
  });
  return ready;
}

async function stopServer(server) {
  if (!server || server.child.exitCode !== null) return;
  const exited = new Promise((done) => server.child.once("exit", done));
  server.child.kill();
  await exited;
}

function sendMail(smtpPort, subject, body, minutesAgo) {
  const date = new Date(Date.now() - minutesAgo * 60000).toUTCString();
  const message = [
    "From: Drill Monitor <probe@example.test>",
    "To: admin@harbor.test",
    `Subject: ${subject}`,
    `Date: ${date}`,
    `Message-ID: <${subject.replace(/\W+/g, "-")}-${Date.now()}@example.test>`,
    "MIME-Version: 1.0",
    "Content-Type: text/plain; charset=utf-8",
    "",
    body,
    "",
  ].join("\r\n");
  return new Promise((accept, reject) => {
    const socket = net.createConnection({ host: "127.0.0.1", port: smtpPort });
    let state = 0;
    const steps = ["EHLO drill.test", "MAIL FROM:<probe@example.test>", "RCPT TO:<admin@harbor.test>", "DATA"];
    socket.setEncoding("utf8");
    socket.on("error", reject);
    const timer = setTimeout(() => reject(new Error("SMTP injection timed out")), 15000);
    socket.on("data", (chunk) => {
      const code = chunk.slice(0, 3);
      if (code === "220" && state === 0) {
        state = 1;
        socket.write(`${steps[0]}\r\n`);
      } else if (state === 1) {
        state = 2;
        socket.write(`${steps[1]}\r\n`);
      } else if (state === 2) {
        state = 3;
        socket.write(`${steps[2]}\r\n`);
      } else if (state === 3) {
        state = 4;
        socket.write(`${steps[3]}\r\n`);
      } else if (state === 4) {
        state = 5;
        socket.write(`${message}\r\n.\r\n`);
      } else if (state === 5) {
        state = 6;
        socket.write("QUIT\r\n");
        clearTimeout(timer);
        socket.end();
        accept();
      }
    });
  });
}

function sqliteSnapshot(file) {
  const python = process.env.HARBOR_DRILL_PYTHON || "python3";
  const script = [
    "import json, sqlite3, sys",
    "db = sqlite3.connect(sys.argv[1])",
    "tables = sorted(r[0] for r in db.execute(\"select name from sqlite_master where type='table'\"))",
    "columns = len(list(db.execute('pragma table_info(letters)')))",
    "subjects = sorted(r[0] for r in db.execute('select subject from letters'))",
    "count = db.execute('select count(*) from letters').fetchone()[0]",
    "print(json.dumps({'count': count, 'subjects': subjects, 'tables': tables, 'lettersColumns': columns}))",
  ].join("\n");
  return JSON.parse(execFileSync(python, ["-c", script, file], { encoding: "utf8" }));
}

function mysqlTarget() {
  const dsn = process.env.HARBOR_DRILL_MYSQL_DSN;
  const match = dsn && dsn.match(/^([^:]+):([^@]*)@tcp\(([^:)]+):(\d+)\)\/([^?]+)/);
  if (!match) throw new Error("HARBOR_DRILL_MYSQL_DSN must look like user:pass@tcp(host:port)/database");
  const [, user, pass, host, port, database] = match;
  return { dsn, user, pass, host, port, database, client: "mysql", dump: "mysqldump" };
}

function mysqlArgs(target) {
  return ["--protocol=TCP", "-h", target.host, "-P", target.port, "-u", target.user, "-N", "-B"];
}

function mysqlEnv(target) {
  return { ...process.env, MYSQL_PWD: target.pass };
}

function runMysql(target, sql, extra = []) {
  return execFileSync(target.client, [...mysqlArgs(target), ...extra, "-e", sql], { encoding: "utf8", env: mysqlEnv(target) });
}

function mysqlSnapshot(target) {
  const tables = runMysql(target, `SELECT table_name FROM information_schema.tables WHERE table_schema='${target.database}' ORDER BY table_name`)
    .split("\n")
    .map((line) => line.trim())
    .filter(Boolean);
  const count = Number(runMysql(target, `SELECT COUNT(*) FROM \`${target.database}\`.letters`).trim());
  const subjects = runMysql(target, `SELECT subject FROM \`${target.database}\`.letters ORDER BY subject`)
    .split("\n")
    .map((line) => line.trim())
    .filter(Boolean);
  const columns = runMysql(target, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema='${target.database}' AND table_name='letters'`).trim();
  return { count, subjects, tables, lettersColumns: Number(columns) };
}

function prepare() {
  const options = parseArgs(process.argv.slice(2));
  const work = mkdtempSync(join(tmpdir(), "harbor-drill-"));
  const state = join(work, "state");
  mkdirSync(state, { recursive: true });
  const oldWorktree = join(work, "old");
  const from = options.from || git(["describe", "--tags", "--abbrev=0", "HEAD^"]);
  git(["worktree", "add", "--detach", oldWorktree, from]);
  const oldBinary = join(work, `old-${binaryName}`);
  const newBinary = join(work, `new-${binaryName}`);
  build(oldWorktree, oldBinary, from);
  const newVersion = process.env.HARBOR_DRILL_TO_VERSION || git(["describe", "--tags", "--always", "HEAD"]);
  build(repo, newBinary, newVersion);
  return { options, work, state, oldWorktree, oldBinary, newBinary, from, newVersion };
}

function datastore(context) {
  const { options, state, work, ...rest } = context;
  if (options.provider === "mysql") {
    const target = mysqlTarget();
    runMysql(target, `DROP DATABASE IF EXISTS \`${target.database}\`; CREATE DATABASE \`${target.database}\``);
    const env = { HARBOR_DB_DRIVER: "mysql", HARBOR_MYSQL_DSN: target.dsn };
    const backup = join(work, "mysql-backup.sql");
    return {
      ...rest,
      options,
      env,
      snapshot: () => mysqlSnapshot(target),
      backup: () => {
        execFileSync(target.dump, [...mysqlArgs(target), "--single-transaction", "--databases", target.database, "--result-file", backup], { stdio: "inherit", env: mysqlEnv(target) });
        return backup;
      },
      restore: (file) => {
        runMysql(target, `DROP DATABASE IF EXISTS \`${target.database}\``);
        execFileSync(target.client, [...mysqlArgs(target), "-e", `CREATE DATABASE \`${target.database}\``], { stdio: "inherit", env: mysqlEnv(target) });
        execFileSync("bash", ["-c", `"${target.client}" ${mysqlArgs(target).join(" ")} < "${file}"`], { stdio: "inherit", env: mysqlEnv(target) });
      },
    };
  }
  const database = join(state, "mail.db");
  const backup = join(work, "sqlite-backup.db");
  return {
    ...rest,
    options,
    env: { HARBOR_DB_DRIVER: "sqlite", HARBOR_DB: database },
    snapshot: () => sqliteSnapshot(database),
    backup: () => {
      const python = process.env.HARBOR_DRILL_PYTHON || "python3";
      rmSync(backup, { force: true });
      // sqlite3.Connection.backup copies committed WAL pages into one consistent file.
      const script = [
        "import sqlite3, sys",
        "source = sqlite3.connect(sys.argv[1])",
        "target = sqlite3.connect(sys.argv[2])",
        "source.backup(target)",
        "target.close()",
        "source.close()",
      ].join("\n");
      execFileSync(python, ["-c", script, database, backup], { stdio: "inherit" });
      return backup;
    },
    restore: (file) => {
      rmSync(database, { force: true });
      rmSync(`${database}-wal`, { force: true });
      rmSync(`${database}-shm`, { force: true });
      copyFileSync(file, database);
    },
  };
}

function environment(context, datastoreEnv) {
  const accounts = {
    "admin@harbor.test": hashPassword(context.oldBinary),
    "ops@harbor.test": hashPassword(context.oldBinary),
  };
  const env = {
    ...process.env,
    ...datastoreEnv,
    HARBOR_DOMAIN: "harbor.test",
    HARBOR_ACCOUNTS: JSON.stringify(accounts),
    HARBOR_ADMINS: "admin@harbor.test",
    HARBOR_DKIM_KEY: "70".repeat(32),
    HARBOR_DIRECT_MX: "false",
    HARBOR_WEB: "127.0.0.1:0",
    HARBOR_SMTP: "127.0.0.1:0",
  };
  delete env.HARBOR_MYSQL_DSN;
  delete env.HARBOR_ARGS;
  if (datastoreEnv.HARBOR_MYSQL_DSN) env.HARBOR_MYSQL_DSN = datastoreEnv.HARBOR_MYSQL_DSN;
  return env;
}

function expect(condition, message) {
  if (!condition) throw new Error(message);
  console.log(`PASS ${message}`);
}

async function main() {
  const context = prepare();
  const store = datastore(context);
  const env = environment(context, store.env);
  console.log(`drill: ${context.from} -> ${context.newVersion} on ${store.options.provider}`);

  let server = await startServer(context.oldBinary, context.state, env);
  for (const [index, subject] of subjects.entries()) {
    await sendMail(server.smtp, subject, `Body for ${subject}.`, 10 + index);
  }
  await new Promise((done) => setTimeout(done, 800));
  const before = store.snapshot();
  await stopServer(server);
  expect(before.count === subjects.length, `old build stored ${before.count} messages`);
  expect(JSON.stringify(before.subjects) === JSON.stringify(subjects), "old build exposes the seeded subjects");
  const backup = store.backup();
  console.log(`backup written: ${backup}`);

  server = await startServer(context.newBinary, context.state, env);
  await new Promise((done) => setTimeout(done, 300));
  const upgraded = store.snapshot();
  await stopServer(server);
  expect(upgraded.count === subjects.length, `new build kept ${upgraded.count} messages after upgrade`);
  expect(JSON.stringify(upgraded.subjects) === JSON.stringify(subjects), "new build exposes the same subjects after upgrade");
  expect(upgraded.lettersColumns >= before.lettersColumns, "letters schema is not narrower after upgrade");
  const added = upgraded.tables.filter((table) => !before.tables.includes(table));
  console.log(`schema tables added by upgrade: ${added.length ? added.join(", ") : "(none)"}`);

  store.restore(backup);
  server = await startServer(context.oldBinary, context.state, env);
  await new Promise((done) => setTimeout(done, 300));
  const restored = store.snapshot();
  await stopServer(server);
  expect(restored.count === subjects.length, `restored backup keeps ${restored.count} messages`);
  expect(JSON.stringify(restored.subjects) === JSON.stringify(subjects), "rollback build reads the restored backup");

  if (!context.options.keep) {
    git(["worktree", "remove", "--force", context.oldWorktree]);
    rmSync(context.work, { recursive: true, force: true });
  } else {
    console.log(`work directory kept: ${context.work}`);
  }
  console.log(`Drill passed for ${store.options.provider}: seed, upgrade, backup, restore, rollback.`);
}

function datastore_port(provider) {
  return provider === "mysql" ? 3306 : 0;
}

main().catch((error) => {
  console.error(`Drill failed: ${error.message}`);
  process.exitCode = 1;
});
