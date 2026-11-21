# Harbor Mail

<img src="web/harbor-mail-logo.png" alt="" width="72" height="72">

Harbor Mail 是面向 Linux 部署的自建域名邮箱，提供中文 Web 邮箱、管理后台和标准邮件协议。Go 程序内嵌前端资源，支持单二进制或 Docker Compose 部署。

## 界面截图

截图来自 Harbor Mail v0.2.6 实际运行的页面，邮件、账号和投递记录均为演示数据。

### 网页邮箱

![Harbor Mail 网页邮箱的文件夹侧栏、收件箱列表和右侧邮件阅读区](docs/assets/webmail.webp)

### 管理后台

![Harbor Mail 管理后台的七日操作趋势、投递状态分布和最近队列任务](docs/assets/admin-overview.webp)

## 功能

| 场景 | 支持内容 |
| --- | --- |
| 网页邮箱 | 多收件人、抄送与密送、富文本、附件、草稿自动保存、定时发送与撤销、授权别名发信、可配置附件上限、签名和模板 |
| 邮件整理 | 嵌套文件夹、标签、规则、全文搜索、保存搜索、稍后处理、批量操作及 EML 导入导出 |
| 邮件协议 | SMTP 收信和提交、IMAP（含 IDLE）、POP3、应用密码及 TLS |
| 管理后台 | 多域名、域管理员、邮箱与别名管理、容量配额、域名 DNS 清单、自动准备 DKIM、上线检查、病毒隔离、投递队列、运行监控、审计和管理员 MFA |
| 数据与部署 | SQLite、MySQL、可选 Redis 后台缓存、旧邮箱 IMAPS 导入、数据库迁移、备份与恢复工具 |

## 优势

- **部署方式灵活。** Linux 单二进制内嵌前端，单实例可用 SQLite。Docker Compose 提供极简、标准和全量三个方案，发布镜像支持 Linux amd64 和 arm64。
- **网页与邮件客户端共用邮箱。** 网页支持定时与撤销发送、邮件规则和标签。桌面和移动客户端可通过 SMTP Submission、IMAP 或 POP3 访问同一邮箱。
- **集中管理多个域名。** 后台可按域委派管理权限，处理邮箱、别名、配额和投递队列，查看登录审计。管理员可启用 TOTP 和恢复码。
- **提供上线和恢复工具。** 支持 Cloudflare DNS-01 签发和续签、内置 DKIM、SQLite 至 MySQL 迁移，以及备份校验和隔离恢复。

## 局限与运维成本

- **公网收发需要额外配置。** 直接从公网接收邮件需要正确的 MX 记录和可达的 TCP 25。向外部邮箱发信默认使用原生 MX 直投，也可配置 SMTP 中继。投递取决于 DNS 认证、出站网络和收件方策略。[接入真实域名](docs/deployment-and-usage.md#接入真实域名)
- **自动垃圾邮件检测和训练依赖 Rspamd。** 基础安装可手动标记垃圾邮件并使用收信规则。可通过 Compose 一键启用 Rspamd 和 ClamAV，也可自行对接。[反垃圾邮件与杀毒部署](docs/spam-and-antivirus.md)
- **网页发信有大小上限。** 每封最多 10 个附件，默认附件合计 6 MiB、整封邮件 10 MiB，可按部署需要分别提高到 28 MiB 和 40 MiB。[邮件大小限制](docs/deployment-and-usage.md#当前范围)
- **SQLite 面向单实例。** 多实例部署使用共享 MySQL，Redis 用于后台统计缓存。邮箱容量、并发和备份耗时需要按部署环境实测。[生产运行手册](ops/README.md)
- **公网环境仍需现场验收。** 仓库中的回归和隔离演练已覆盖数据库及核心流程。实际域名的公网收发、证书续签和异地灾难恢复需要在部署环境验证。[生产运行手册](ops/README.md)

## Docker 快速部署

| 安装方案 | 包含组件 |
| --- | --- |
| [全量版](docs/deployment-and-usage.md#全量版) | Harbor Mail、MySQL、Redis、Rspamd、ClamAV，使用 [单文件 Compose](compose.full.yaml) |
| [标准版（默认）](docs/deployment-and-usage.md#标准版默认) | Harbor Mail、MySQL、Redis，使用 [单文件 Compose](compose.standard.yaml) |
| [极简版](docs/deployment-and-usage.md#极简版) | Harbor Mail、内置 SQLite，使用 [单文件 Compose](compose.minimal.yaml) |

先[下载部署文件](docs/deployment-and-usage.md#准备部署文件)，再任选一个方案创建管理员并启动。

需要公网 HTTPS 时，可用 Cloudflare DNS-01 自动签发证书，Web 端口可自行选择。 [Cloudflare 自动证书](docs/deployment-and-usage.md#cloudflare-dns-01)

需要切换到已有的域名邮箱时，可使用带续传的 IMAPS 迁移命令。 [旧邮箱迁移](docs/deployment-and-usage.md#从旧-imap-邮箱迁移)

## Linux 单二进制启动

| 安装方案 | 包含组件 |
| --- | --- |
| [全量版](ops/README.md#单二进制全量版) | Harbor Mail、MySQL、Redis、Rspamd、ClamAV |
| [标准版](ops/README.md#单二进制标准版) | Harbor Mail、MySQL、Redis |
| [极简版](ops/README.md#单二进制极简版) | Harbor Mail、内置 SQLite |

三个方案共用同架构的 `harbormail` 和 systemd 服务模板。MySQL、Redis 与扫描组件由 Linux 主机或已有服务提供。

从 [GitHub Releases](https://github.com/makessr/HarborMail/releases/latest) 下载 `harbormail-linux-amd64` 或 `harbormail-linux-arm64`。同一版本附带 `SHA256SUMS` 和 `harbormail-systemd.tar.gz`。以 amd64 为例，在二进制所在目录运行：

```bash
sha256sum --ignore-missing -c SHA256SUMS
install -m 0755 harbormail-linux-amd64 ./harbormail
```

如需从源码构建，在有 Go 1.27.4 或更新版本的构建机上执行：

```bash
git clone https://github.com/makessr/HarborMail.git
cd HarborMail
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o harbormail ./cmd/harbormail
```

ARM64 源码构建把 `GOARCH=amd64` 改为 `GOARCH=arm64`。Go 仅用于构建。准备好二进制后，[创建首个账号](docs/deployment-and-usage.md#创建首个-linux-邮箱)，再按上表配置所选方案。

长期运行请安装 `harbormail.service`，再用 `sudo systemctl enable --now harbormail.service` 启动并开启开机自启。[systemd 安装](ops/README.md#linux-单二进制-systemd-部署) · [状态、启动、重启、停止和日志](ops/README.md#日常服务命令)

## 开发与测试

在源码目录运行 `go test ./...`。Windows 可用于本地开发和构建 Linux 目标，详见 [Windows 开发说明](docs/deployment-and-usage.md#windows-开发)。

GitHub 的 `Verify Harbor Mail` 工作流统一执行 Go 回归、SQLite 与 MySQL 8.4 验证、前端和运维脚本语法检查及 Prometheus 告警规则测试。拉取请求与镜像、二进制发布都会运行这套检查，也可从 Actions 手动运行。

`Publish Harbor Mail image` 检查通过后会并行构建 GHCR 镜像和同一提交的单文件程序。GHCR 固定提交标签包含 Linux amd64 和 arm64，两个架构的容器均通过启动二进制检查后才更新 `latest`。Actions 的 **Artifacts** 用于验收，下载需要登录 GitHub，文件会到期。推送 `vMAJOR.MINOR.PATCH` 标签后，Linux amd64、Linux arm64、Windows amd64、systemd 模板和 `SHA256SUMS` 会发布到 [GitHub Releases](https://github.com/makessr/HarborMail/releases/latest)，可公开下载。

| 目录 | 内容 |
| --- | --- |
| `cmd/harbormail/` | 单二进制入口 |
| `internal/server/` | Web API、邮件协议、邮箱业务、数据存储及相应的 Go 测试 |
| `internal/processstats/` | Linux、Windows 的进程资源指标 |
| `web/` | 邮箱界面及浏览器静态资源 |
| `ops/` | 备份、监控和其他运维配置 |
| `docs/` | 部署指南及文档资源 |

`compose*.yaml` 和 `Dockerfile` 位于仓库根目录。

## 文档与验收

- [部署与使用指南](docs/deployment-and-usage.md)涵盖配置、邮件客户端、Web 功能和数据库迁移。
- [生产运行手册](ops/README.md)涵盖 Linux 部署、站点验收和监控告警，另见 [从 Restic 导出恢复包](ops/README.md#从-restic-导出恢复包)。
- [接入真实域名](docs/deployment-and-usage.md#接入真实域名)涵盖 DNS、邮件 TLS、中继和公网接入。

已在本地 Docker 验证 MySQL 与 Redis 部署、SMTP 收信、Redis 停机回源及双实例网页同步。
