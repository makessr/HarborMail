"use strict";

const runtimeMiB = 1024 * 1024;
const runtimeRefreshMillis = 10000;
const runtimeHistoryMillis = 5 * 60 * 1000;

function setupAdminRuntime() {
  const pause = adminElement("admin-runtime-pause");
  const updated = adminElement("admin-runtime-updated");
  const cpu = adminElement("admin-runtime-cpu");
  const memory = adminElement("admin-runtime-ram");
  const clock = new Intl.DateTimeFormat("zh-CN", { timeStyle: "medium" });
  let samples = [];
  let previous;
  let timer;
  let paused = false;
  let loading = false;

  const mib = value => value == null ? "—" : (value / runtimeMiB).toFixed(1) + " MiB";
  const duration = seconds => {
    if (seconds < 60) return seconds + " 秒";
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return minutes + " 分钟";
    if (minutes < 1440) return Math.floor(minutes / 60) + " 小时 " + (minutes % 60) + " 分钟";
    return Math.floor(minutes / 1440) + " 天 " + Math.floor(minutes % 1440 / 60) + " 小时";
  };

  function draw() {
    drawRuntimeChart(cpu, samples, [{ key: "cpu", color: "--chart-blue" }], 1, "%");
    drawRuntimeChart(memory, samples, [
      { key: "resident", color: "--chart-blue" }, { key: "heap", color: "--chart-teal" }
    ], 1, " MiB");
  }
  const observer = new ResizeObserver(draw);
  observer.observe(cpu);
  observer.observe(memory);
  new MutationObserver(draw).observe(document.documentElement, { attributeFilter: ["data-theme"] });

  function display(data) {
    const sameProcess = previous?.startedAt === data.startedAt;
    if (previous && !sameProcess) samples = [];
    const elapsed = sameProcess ? data.at - previous.at : 0;
    const cpuMillis = data.process.cpuMillis;
    let percent = null;
    if (elapsed > 0 && elapsed < 3 * runtimeRefreshMillis && cpuMillis != null &&
        previous.process.cpuMillis != null && cpuMillis >= previous.process.cpuMillis) {
      percent = (cpuMillis - previous.process.cpuMillis) / elapsed * 100;
    }
    samples.push({
      at: data.at, cpu: percent,
      resident: data.process.residentBytes == null ? null : data.process.residentBytes / runtimeMiB,
      heap: data.heapBytes / runtimeMiB
    });
    samples = samples.filter(sample => sample.at >= data.at - runtimeHistoryMillis).slice(-30);
    previous = data;
    adminElement("admin-identity").textContent = data.email;
    const cpuLabel = cpuMillis == null ? "不可用" : percent == null ? "等待下一次采样" : percent.toFixed(1) + "%";
    renderDetails("admin-runtime-kpis", [
      ["进程 CPU", cpuLabel], ["常驻内存", mib(data.process.residentBytes)],
      ["Go 堆对象", mib(data.heapBytes)], ["运行时长", duration(Math.max(0, Math.floor((data.at - data.startedAt) / 1000)))]
    ]);
    const db = data.database;
    renderDetails("admin-runtime-db", [
      ["数据库", db.driver === "mysql" ? "MySQL" : "SQLite"],
      ["探测状态", db.ready ? "可用（" + db.probeMillis.toFixed(1) + " ms）" : "探测失败"],
      ["打开连接 / 上限", db.open + " / " + (db.limit || "不限")],
      ["使用中 / 空闲", db.inUse + " / " + db.idle],
      ["累计等待", db.waitCount + " 次，共 " + db.waitMillis + " ms"]
    ]);
    renderDetails("admin-runtime-workers", [
      ["Go 协程", data.goroutines], ["GC 次数", data.gcCycles], ["Go 并行度", data.cpuParallelism],
      ["发件队列", data.outboxPaused ? "暂停" : "运行中"],
      ["最久队列等待", data.mail ? duration(data.mail.oldestDueSeconds) : "不可用"],
      ["投递结果待确认", data.mail?.uncertain ?? "不可用"],
      ["训练失败样本", data.mail?.failedTraining ?? "不可用"]
    ]);
    activePanel.hidden = false;
    statusOutput.textContent = "";
    updated.textContent = "更新于 " + clock.format(data.at) + " · " +
      (paused ? "自动刷新已暂停" : "每 10 秒刷新，保留最近 30 个样本");
    draw();
  }

  function schedule() {
    clearTimeout(timer);
    if (!paused && !document.hidden) timer = setTimeout(load, runtimeRefreshMillis);
  }

  async function load() {
    if (loading) return;
    clearTimeout(timer);
    loading = refresh.disabled = true;
    if (activePanel.hidden) statusOutput.textContent = "正在读取运行指标…";
    try {
      const response = await fetch("/api/admin/runtime");
      if (response.status === 401 || response.status === 403) {
        paused = true;
        location.replace("/");
        return;
      }
      if (!response.ok) throw new Error();
      display(await response.json());
    } catch {
      statusOutput.textContent = "本次采样失败，检查服务后可以刷新重试。";
    } finally {
      loading = refresh.disabled = false;
      schedule();
    }
  }

  pause.addEventListener("click", () => {
    paused = !paused;
    pause.textContent = paused ? "恢复刷新" : "暂停刷新";
    if (paused) {
      clearTimeout(timer);
      updated.textContent = "自动刷新已暂停。";
    } else {
      void load();
    }
  });
  document.addEventListener("visibilitychange", () => {
    clearTimeout(timer);
    if (!document.hidden && !paused) void load();
  });
  return load;
}

function drawRuntimeChart(canvas, samples, series, minimum, unit) {
  const width = canvas.clientWidth;
  const height = canvas.clientHeight;
  if (!width || !height) return;
  const scale = window.devicePixelRatio || 1;
  canvas.width = Math.round(width * scale);
  canvas.height = Math.round(height * scale);
  const ctx = canvas.getContext("2d");
  ctx.scale(scale, scale);
  const style = getComputedStyle(canvas);
  const left = 56, right = width - 12, top = 14, bottom = height - 25;
  const peak = Math.max(minimum, ...samples.flatMap(sample => series.map(line => sample[line.key] ?? 0)));
  const ceiling = Math.ceil(peak);
  ctx.strokeStyle = style.getPropertyValue("--line");
  ctx.fillStyle = style.getPropertyValue("--muted");
  ctx.font = "11px system-ui";
  ctx.setLineDash([3, 4]);
  for (let step = 0; step <= 4; step++) {
    const y = top + (bottom - top) * step / 4;
    ctx.beginPath();
    ctx.moveTo(left, y);
    ctx.lineTo(right, y);
    ctx.stroke();
    ctx.fillText(Number((ceiling * (1 - step / 4)).toFixed(1)) + unit, 2, y + 4);
  }
  ctx.setLineDash([]);
  if (samples.length === 0) return;
  const end = samples.at(-1).at;
  const start = Math.min(end - runtimeRefreshMillis, samples[0].at);
  for (const line of series) {
    ctx.strokeStyle = ctx.fillStyle = style.getPropertyValue(line.color).trim();
    ctx.lineWidth = 2;
    ctx.lineCap = "round";
    let previous;
    for (const sample of samples) {
      const value = sample[line.key];
      if (value == null) { previous = null; continue; }
      const point = {
        x: left + (sample.at - start) / (end - start) * (right - left),
        y: bottom - value / ceiling * (bottom - top),
        at: sample.at
      };
      ctx.beginPath();
      if (previous && sample.at - previous.at < 3 * runtimeRefreshMillis) {
        ctx.moveTo(previous.x, previous.y);
        ctx.lineTo(point.x, point.y);
        ctx.stroke();
      }
      ctx.beginPath();
      ctx.arc(point.x, point.y, 2.3, 0, 2 * Math.PI);
      ctx.fill();
      previous = point;
    }
  }
  ctx.fillStyle = style.getPropertyValue("--muted");
  ctx.fillText(new Date(start).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }), left, height - 5);
  ctx.textAlign = "right";
  ctx.fillText("最新", right, height - 5);
}
