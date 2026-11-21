"use strict";

function overviewLink(label, href) {
  const link = document.createElement("a");
  link.textContent = label;
  link.href = href;
  return link;
}

function renderAdminOverview(data) {
  adminElement("admin-build-version").textContent = data.version === "dev" || !data.version ? "开发构建" : data.version;
  const queue = data.queue;
  const active = ["queued", "scheduled", "undo", "preparing", "sending"].reduce((sum, key) => sum + (queue[key] || 0), 0);
  const waiting = data.oldestDue ? Math.max(0, data.generatedAt - data.oldestDue) : 0;
  renderDetails("admin-kpis", [
    ["待处理任务", overviewLink(active, "/admin/queue")],
    ["异常任务", overviewLink((queue.failed || 0) + (queue.uncertain || 0), "#admin-queue-title")],
    ["最久到期等待", !data.oldestDue ? "—" : waiting < 60 ? "不到 1 分钟" :
      waiting < 3600 ? Math.floor(waiting / 60) + " 分钟" : Math.floor(waiting / 3600) + " 小时"],
    ["邮箱账号", overviewLink(data.accounts.length, "/admin/accounts")]
  ]);
  renderQueueBars(queue);
  renderAdminTrend(data.trend);
  adminElement("admin-recent").replaceChildren(...data.recent.map(job => {
    const link = overviewLink("#" + job.id, "/admin/queue?" + new URLSearchParams({ id: job.id }));
    const next = ["queued", "scheduled", "undo"].includes(job.state) ? queueTime(job.due) : "—";
    return deliveryRow([link, job.sender, queueLabels[job.state] || job.state, job.attempts, next, job.reason || "—"]);
  }));
  adminElement("admin-recent-empty").hidden = data.recent.length > 0;
  renderDetails("admin-service", [
    ["SMTP 服务主机", data.domain], ["数据库", data.database === "mysql" ? "MySQL" : "SQLite"],
    ["收信域名", overviewLink(data.domains.length, "/admin/domains")],
    ["外发投递", data.directMX ? (data.relayConfigured ? "SMTP 中继 / MX 直投" : "MX 直投") : data.relayConfigured ? "SMTP 中继" : "仅站内"],
    ["垃圾邮件扫描", data.spamScanning ? "已配置" : "未配置"]
  ]);
  adminElement("admin-overview-updated").textContent = "更新于 " + queueTime(data.generatedAt);
}

function renderQueueBars(queue) {
  const max = Math.max(1, ...Object.values(queue));
  const rows = Object.entries(queueLabels).map(([state, label]) => {
    const count = queue[state] || 0;
    const item = document.createElement("li");
    const link = overviewLink(label, "/admin/queue?" + new URLSearchParams({ state }));
    link.setAttribute("aria-label", label + " " + count + " 项，查看任务");
    const number = document.createElement("span");
    number.textContent = count;
    const track = document.createElement("div");
    track.className = "admin-bar-track";
    track.setAttribute("aria-hidden", "true");
    const bar = document.createElement("span");
    bar.className = state === "failed" || state === "uncertain" ? "chart-alert" : "chart-account";
    bar.style.width = (count / max * 100) + "%";
    track.append(bar);
    item.append(link, track, number);
    return item;
  });
  adminElement("admin-queue").replaceChildren(...rows);
}

function renderAdminTrend(days) {
  const totals = days.map(day => day.account + day.aliases + day.retry);
  const max = Math.max(4, Math.ceil(Math.max(...totals) / 4) * 4);
  adminElement("admin-trend-axis").replaceChildren(...Array.from({ length: 5 }, (_, index) => {
    const tick = document.createElement("li");
    tick.textContent = max * (4 - index) / 4;
    return tick;
  }));
  const formatter = new Intl.DateTimeFormat("zh-CN", { month: "numeric", day: "numeric", timeZone: "UTC" });
  const rows = days.map((day, index) => {
    const item = document.createElement("li");
    const date = formatter.format(day.day * 1000);
    const breakdown = date + "，账号与域名 " + day.account + "，别名变更 " + day.aliases + "，投递重试 " + day.retry;
    const label = document.createElement("span");
    label.className = "visually-hidden";
    label.textContent = breakdown;
    item.title = breakdown;
    const plot = document.createElement("div");
    plot.className = "admin-trend-stack";
    plot.setAttribute("aria-hidden", "true");
    for (const field of ["account", "aliases", "retry"]) {
      if (!day[field]) continue;
      const part = document.createElement("span");
      part.className = "chart-" + field;
      part.style.height = (day[field] / max * 100) + "%";
      plot.append(part);
    }
    const total = document.createElement("span");
    total.textContent = totals[index] || "";
    total.setAttribute("aria-hidden", "true");
    const time = document.createElement("time");
    time.textContent = date;
    time.dateTime = new Date(day.day * 1000).toISOString().slice(0, 10);
    time.setAttribute("aria-hidden", "true");
    item.append(label, total, plot, time);
    return item;
  });
  adminElement("admin-trend").replaceChildren(...rows);
}
