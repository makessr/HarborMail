<div align="center">
  <img src="web/harbor-mail-logo.png" width="88" height="88" alt="Harbor Mail 标志">
  <h1>Harbor Mail</h1>
  <p><strong>用自己的域名收发邮件</strong></p>
  <p>中文网页邮箱 · 多域名管理 · SMTP、IMAP、POP3</p>
  <p>
    <a href="https://github.com/makessr/HarborMail/releases/latest"><strong>下载正式版</strong></a>
    &nbsp;·&nbsp;
    <a href="docs/deploy/README.md"><strong>部署与升级</strong></a>
    &nbsp;·&nbsp;
    <a href="docs/releases/v0.2.25.md">v0.2.25 发行说明</a>
  </p>
</div>

Harbor Mail 是一个用 Go 编写的自建域名邮箱。用户从网页收发邮件，管理员在 `/admin` 管理域名、邮箱、配额和投递队列。可以用 Docker Compose 启动，也可以下载 Linux 二进制交给 systemd 管理。

[快速启动](#docker-首次启动) · [部署选型](#部署方案和难度) · [功能](#功能与取舍) · [界面截图](#界面演示) · [开发](#底层实现)

## 快速开始

### Docker 首次启动

新用户建议从 **Docker 标准版**开始。在安装了 Git、curl、OpenSSL 和 Docker Compose 的 Linux 服务器上执行。初始化时输入管理员邮箱和密码。

```bash
release_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' https://github.com/makessr/HarborMail/releases/latest) &&
version=${release_url##*/} &&
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] &&
git clone --branch "$version" --depth 1 https://github.com/makessr/HarborMail.git &&
  cd HarborMail &&
  sudo bash ops/init-docker.sh standard &&
  sudo docker compose config --quiet &&
  sudo docker compose up -d --no-build --wait --wait-timeout 900 &&
  curl -fsS http://127.0.0.1:8080/healthz
```

健康接口返回 `ok` 后，从本机回环地址打开后台。远程服务器可以通过 SSH 隧道访问，再用刚创建的账号给自己发一封测试邮件。 [打开后台并试发](docs/deploy/common.md#打开后台并试发) · [接入公网](docs/quick-start.md#接入公网)

### Linux 单二进制启动

从 [GitHub Release](https://github.com/makessr/HarborMail/releases/latest) 下载对应的 Linux amd64 或 arm64 文件，再按所选方案安装 systemd 服务。 [下载和校验步骤](docs/deploy/common.md#下载同版二进制)

从源码构建时，在仓库根目录使用 Go 1.27.4 或更新版本执行 `go build -o harbormail ./cmd/harbormail`。网页资源已内嵌在二进制中。

## 部署方案和难度

| 方案 | 适合谁 | Docker | Linux 二进制 |
| --- | --- | --- | --- |
| **精简版** | 个人邮箱或轻量单实例，使用内置 SQLite | [部署](docs/deploy/docker-minimal-install.md) · [升级](docs/deploy/docker-minimal-upgrade.md) | [部署](docs/deploy/binary-minimal-install.md) · [升级](docs/deploy/binary-minimal-upgrade.md) |
| **标准版** | 首次长期部署或小团队，使用 MySQL 8.4 和 Redis | [部署](docs/deploy/docker-standard-install.md) · [升级](docs/deploy/docker-standard-upgrade.md) | [部署](docs/deploy/binary-standard-install.md) · [升级](docs/deploy/binary-standard-upgrade.md) |
| **全量版** | 需要来信反垃圾和病毒隔离，在标准版上接入 Rspamd 和 ClamAV | [部署](docs/deploy/docker-full-install.md) · [升级](docs/deploy/docker-full-upgrade.md) | [部署](docs/deploy/binary-full-install.md) · [升级](docs/deploy/binary-full-upgrade.md) |

Docker 自动启动配套服务。Linux 三种方案使用同一发行二进制，标准版和全量版需要准备相应的外部服务。

<details>
<summary>查看服务器配置与上线清单</summary>

### 服务器配置推荐

以下是小规模单机部署的起配建议。内存按整台 Linux 主机估算，SSD 容量需要容纳系统、镜像和在线数据，备份另计。

| 部署方案 | CPU | 内存 | SSD | 适用场景 |
| --- | ---: | ---: | ---: | --- |
| 极简版 · SQLite | 1–2 vCPU | 1–2 GiB | 30–50 GiB | 个人邮箱、演示或低并发单实例 |
| 标准版 · MySQL + Redis | 2 vCPU | 4 GiB | 60–100 GiB | 小团队、多个域名，或同机运行 1Panel 和反向代理 |
| 全量版 · 含 Rspamd + ClamAV | 4 vCPU | 8 GiB | 100–200 GiB | 长期开启反垃圾和病毒扫描，并为病毒库更新留出余量 |

**2 核 4 GiB 优先选标准版。** ClamAV 官方为扫描组件本身建议预留 3–4 GiB 内存，病毒库重载时占用会上升。它与 MySQL、1Panel 同机运行时，按全量版起配更合适。 [ClamAV 容器内存说明](https://docs.clamav.net/manual/Installing/Docker.html#memory-ram-requirements)

这些是配置建议，实测范围见[下方测试数据](#实机测试数据)。磁盘按邮箱实际用量、附件增长、索引和数据库日志扩容，并使用独立位置保存备份。 [生产存储与容量规划](ops/README.md#存储上限)

### 配置清单

| 需要准备 | 说明 |
| --- | --- |
| Linux 主机 | Docker 版需要 Docker Engine 与 Compose。原生版可用发行包和 systemd |
| 管理员 | 准备邮箱地址和密码。Docker 初始化生成 `.env` 和 `secrets/`，原生服务读取 `/etc/harbormail/harbormail.env` 和凭据文件 |
| 邮件域名和主机名 | 例如 `example.com` 和 `mail.example.com`。配置 A 或 AAAA、MX、SPF、DKIM 和 DMARC，核对主机商提供的 PTR |
| 端口 | 默认 Web `8080`、收信 `2525`，两者绑定回环。公网 Web 通常经 HTTPS `443`，SMTP 收信走 TCP `25`，直投需要出站 TCP `25`。按需开放客户端 `465/587`、`993` 或 `995` |
| TLS | 可用反向代理、已有 PEM 或 ACME。用 Cloudflare DNS-01 时需准备对应域的 API 令牌 |
| 持久存储 | 默认使用命名卷。首次启动前可用 `HARBOR_DATA_PATH` 和 `HARBOR_MYSQL_DATA_PATH` 指定宿主机目录，极简版只需前者。备份数据库、数据目录、配置、凭据和 DKIM 密钥 |
| 标准版与全量版 | Docker 初始化会准备 MySQL 凭据。原生部署需提供 MySQL DSN 和 Redis 地址，全量版还需要扫描组件及策略 |

已有实例更换存储路径前，先备份并迁移原数据。 [存储路径配置](docs/quick-start.md#选择数据存放位置) · [接入公网域名](docs/deployment-and-usage.md#接入真实域名) · [扫描配置](docs/spam-and-antivirus.md)

</details>

## 界面演示

截图来自正式版 **v0.2.18** 的 Docker 标准版。账号、域名、IP 和邮件均为演示数据。

### 网页邮箱

侧栏可切换收件箱、草稿、发件箱和自定义文件夹。阅读页可查看发件人详情、下载原始邮件、打印、回复或转发。

![Harbor Mail v0.2.18 网页邮箱，展示收件箱与邮件阅读页](docs/assets/webmail.webp)

<details>
<summary>查看更多实机截图 · 管理后台、DNS、写信和运行监控</summary>

### 管理后台

概览显示账号数、近期管理操作和投递队列。后台也提供别名、配额、扫描隔离与审计管理。

![Harbor Mail v0.2.18 管理后台的概览、趋势和队列](docs/assets/admin-overview.webp)

### 域名与 DNS

新增邮件域名后，从「DNS 记录」查看并复制 A、MX、SPF、DKIM 和 DMARC 配置。发布记录后可在后台运行域名上线检查。

![Harbor Mail v0.2.18 域名 DNS 记录生成界面](docs/assets/admin-dns.webp)

### 写邮件

写信窗口可展开抄送和密送，并提供附件、草稿和定时发送入口。

![Harbor Mail 写邮件窗口，包含正文、主题和发送工具](docs/assets/webmail-compose.webp)

### 账号管理

按域名、角色和状态筛选账号，打开详情查看可用管理操作。

![Harbor Mail 账号列表和账号详情面板](docs/assets/admin-accounts.webp)

### 存储用量

查看单个邮箱的邮件、文件夹和投递队列用量，并管理容量预警。

![Harbor Mail 存储用量和容量预警页面](docs/assets/admin-storage.webp)

### 运行监控

查看 Harbor 进程 CPU、内存、数据库连接池及后台处理状态。

![Harbor Mail 运行监控的 CPU、内存和数据库指标](docs/assets/admin-runtime.webp)

</details>

## 功能与取舍

| 场景 | 支持内容 |
| --- | --- |
| 网页收发 | 多收件人、抄送和密送、附件、草稿自动保存、定时发送和撤销、授权别名发信、签名与模板 |
| 邮件整理 | 嵌套文件夹、标签、邮件规则、搜索、稍后处理、EML 导入和导出 |
| 邮件客户端 | SMTP 收信与提交、IMAP（含 IDLE）、POP3、TLS、应用密码 |
| 登录保护 | 网页邮箱两步验证、一次性恢复码、会话管理 |
| 域名和运维 | 多域名、管理员与域管理员、邮箱和别名、DNS 指引、内置 DKIM、投递队列、存储配额、隔离与审计 |
| 可选集成 | SMTP 中继、Cloudflare DNS-01 自动证书、Rspamd 和 ClamAV、旧邮箱 IMAPS 迁移 |

**来信扫描与病毒隔离**。全站管理员可在「安全 → 公网部署安全检查」测试扫描接口，在「隔离邮件」查看和处置隔离记录。自动反垃圾依赖 Rspamd，病毒检测由 Rspamd 调用 ClamAV，启用隔离策略后才会产生病毒隔离记录。Docker 全量版已预配这些组件。 [扫描配置与处置说明](docs/spam-and-antivirus.md)

**优点**

- 网页和 IMAP 客户端共用邮箱。可以从 SQLite 单实例起步，也可部署 MySQL、Redis 和来信扫描。
- 多个域名可以在同一后台维护。默认启用原生 MX 直投，也可配置 SMTP 中继。
- 邮箱数据、扫描数据和凭据可独立备份。Docker 默认使用命名卷，当前仓库的 Compose 也支持指定宿主机路径。

**局限**

- 公网收发涉及域名 DNS、证书和邮件端口。直投还取决于出站 TCP 25、IP 信誉和收件方策略。远端返回 SMTP `250` 表示已接收，最终分类由对方决定。
- SQLite 适合单实例。共享 MySQL 时需要维护数据库连接、备份和各实例配置。
- Rspamd 和 ClamAV 增加内存、磁盘及病毒库维护开销。下方的标准版测试未启用扫描。
- 网页发信默认附件合计 6 MiB，编码后整封邮件上限 10 MiB。可配置的最高值分别为 28 MiB 和 40 MiB。 [邮件大小与附件](docs/deployment-and-usage.md#当前范围)

## 升级已有部署

在「管理后台 → 概览 → 关于 Harbor Mail」点击「检查更新」。升级前备份数据库、配置和密钥。升级后检查后台版本、`/healthz` 和实际邮件收发。 [备份与恢复](ops/README.md#备份)

### Docker 容器升级

沿用原项目名、Compose 配置和数据挂载。确认备份后，仅更新 `harbor` 服务。选择对应的 [精简版](docs/deploy/docker-minimal-upgrade.md)、[标准版](docs/deploy/docker-standard-upgrade.md)或[全量版](docs/deploy/docker-full-upgrade.md)升级指南。

### Linux 单二进制升级

按 [精简版](docs/deploy/binary-minimal-upgrade.md)、[标准版](docs/deploy/binary-standard-upgrade.md)或[全量版](docs/deploy/binary-full-upgrade.md)完成备份，再[校验并替换二进制](docs/deploy/common.md#二进制替换与回退)。

## 实机测试数据

<details>
<summary>查看 Docker 精简版和标准版的轻负载记录</summary>

2026-11-22，正式版 v0.2.18，Docker Desktop Linux 环境（Docker 可见 12 个逻辑 CPU 和 15.58 GiB 内存）。全部请求走本机回环。每种方案投递 6 封合成邮件，预热 10 次后串行请求 `/healthz` 100 次，并用 `docker stats --no-stream` 记录容器内存快照。

| 验证项目 | 极简版 · SQLite | 标准版 · MySQL 8.4 + Redis |
| --- | ---: | ---: |
| SMTP 投递后网页收件箱可见 | 6 / 6 | 6 / 6 |
| `/healthz` 成功 | 100 / 100 | 100 / 100 |
| `/healthz` P50 / P95 | 0.65 / 1.26 ms | 0.75 / 1.18 ms |
| Harbor 容器内存 | 10.94 MiB | 9.56 MiB |
| 数据库与缓存容器内存 | 内置 SQLite | MySQL 463.9 MiB、Redis 6.95 MiB |

标准版停止 Redis 后，后台概览继续从 MySQL 读取，测试结束后恢复 Redis。表中是轻负载下的容器内存快照，运行监控截图则显示另一时点的进程常驻内存（RSS）。健康接口的延迟也不能代表邮件投递速度。全量扫描、公网投递和高并发峰值未纳入这组数字。

</details>

## 发布平台和架构

| 产物 | 架构 | 发布状态 |
| --- | --- | --- |
| Docker 镜像 | Linux amd64、arm64 | GHCR `ghcr.io/makessr/harbormail:latest` |
| Linux 单二进制 | amd64、arm64 | 正式版提供对应可执行文件、systemd 包和 `SHA256SUMS` |
| Windows 本地开发 | amd64、arm64 | 自 v0.2.19 起，正式版提供两种架构，后台按运行架构选择下载 |

从主分支提交 `39080e8` 起，正式发布并行构建二进制和双架构镜像。镜像验证通过并更新 `latest` 后，才公开 GitHub Release。 [发布工作流](.github/workflows/publish-binaries.yaml)

## 底层实现

| 层次 | 实现 |
| --- | --- |
| 服务与协议 | Go 1.27.4。Web API、SMTP、IMAP、POP3、投递队列和 MIME 处理在同一个服务中 |
| 网页 | HTML、CSS、原生 JavaScript 和 Tailwind 生成样式。Go `embed` 内嵌资源，运行服务器无需 Node.js |
| 数据 | 内置 SQLite 使用纯 Go 驱动 `modernc.org/sqlite`。标准与全量部署使用 MySQL，Redis 提供后台缓存 |
| 实时更新 | 网页通过 SSE 接收邮箱变化。IMAP 客户端可用 IDLE |
| 运维 | Compose、systemd、ACME、备份与恢复。标准版与全量版的 MySQL、Redis 默认留在 Compose 网络内 |

`cmd/harbormail/` 是程序入口，`internal/server/` 包含业务、协议和测试，`web/` 是网页资源，`ui/` 是样式源码，`ops/` 是运维文件，`docs/` 是文档。

本地回归执行 `go test ./...`。修改 Tailwind 样式后，用 `npm ci && npm run build:ui` 生成前端样式。 [GitHub CI](https://github.com/makessr/HarborMail/actions/workflows/verify.yaml)

修改协议、邮件存储或投递流程时，参考[开发与验证](docs/development.md)。SMTP Submission 的并发和内存预算、POP3 历史邮件尺寸回填及客户端端口核对见[生产运行手册](ops/README.md)。安全问题请通过[安全报告入口](SECURITY.md)提交。

Submission 容量计数可从 Prometheus 指标读取。MySQL 上，已有 UID 的 IMAP 文件夹使用只读一致性快照，缺失 UID 时仍由现有分配器补齐。

[v0.2.25 发行说明](docs/releases/v0.2.25.md)涵盖网页改密与找回密码、回收邮箱释放地址、后台存储列表，以及全量 Compose 的本机备份恢复与巡检。

参与开发见[贡献指南](CONTRIBUTING.md)。发布维护见[发布检查清单](docs/release-checklist.md)。

## 许可证

Harbor Mail 自有代码采用 [Apache-2.0](LICENSE)。第三方组件按各自条款分发。 [第三方许可声明](THIRD_PARTY_NOTICES.md)
