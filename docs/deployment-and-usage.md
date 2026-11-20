# Harbor Mail 部署与使用指南

<img src="../web/harbor-mail-logo.png" alt="" width="72" height="72">

首次安装从[三步 Docker 部署](#docker-compose-部署)开始。已有部署可直接查看[配置项](#配置)、[邮件客户端](#接入邮件客户端)或[生产运行手册](../ops/README.md)。

## 本地启动

### Linux 单二进制

从 [GitHub Releases](https://github.com/makessr/HarborMail/releases/latest) 下载目标架构的二进制，或按[项目首页](../README.md#linux-单二进制启动)从源码构建。将可执行文件命名为 `harbormail`，放在 Linux 服务账号可运行的目录。以下 Bash 示例用于首次生成账号配置，已有配置可直接复用。

```bash
(
  set -e
  umask 077
  mkdir -p secrets
  read -rsp '输入邮箱密码（12–72 字节）: ' mailbox_password
  printf '\n'
  hash=$(printf '%s\n' "$mailbox_password" | ./harbormail hash-password)
  unset mailbox_password
  printf '{"hello@harbor.test":"%s"}\n' "$hash" > secrets/accounts.json
)
HARBOR_ACCOUNTS_FILE="$PWD/secrets/accounts.json" ./harbormail
```

默认监听 `127.0.0.1:8080`（Web）和 `127.0.0.1:2525`（SMTP），SQLite 会在工作目录下创建 `data/harbor.db`。可以用 `HARBOR_DB` 指定持久数据路径，以服务账号运行并保证其可写。使用 MySQL 时按[数据库配置](#使用-mysql)设置环境变量。远程部署时按[接入真实域名](#接入真实域名)配置 Web 反向代理和 SMTP。

长期部署见 [Linux 单二进制 systemd 部署](../ops/README.md#linux-单二进制-systemd-部署)。

### Windows 开发

Windows 用于本地开发。可从 [GitHub Releases](https://github.com/makessr/HarborMail/releases/latest) 下载 `harbormail-windows-amd64.exe`，重命名为 `harbormail.exe`，然后从下面的 `$accountHash` 一行开始。若从源码构建，需要 Go 1.27.4 或更新版本。以下命令从项目目录在 PowerShell 7 中运行。

```powershell
go build -o harbormail.exe ./cmd/harbormail
$accountHash = Read-Host '输入用于本地邮箱的密码（12–72 字节）' -MaskInput | .\harbormail.exe hash-password
if ($LASTEXITCODE -ne 0) { throw '密码哈希生成失败' }
$env:HARBOR_ACCOUNTS = @{ 'hello@harbor.test' = $accountHash } | ConvertTo-Json -Compress
.\harbormail.exe
```

打开 [本地 Web 邮箱](http://127.0.0.1:8080)，使用 `hello@harbor.test` 和刚设置的密码登录。给自己发一封邮件，然后切换到收件箱，即可验证基本收发。已有旧安装的 PowerShell 用户可以通过 `pwsh` 启动 PowerShell 7。

账号地址使用小写，账号列表随服务启动读取。删除账号或替换其密码哈希后，旧登录会话失效。

## Docker Compose 部署

适用于首次在 Linux amd64 或 arm64 服务器部署。需要 Docker Engine、Compose 插件、Git、OpenSSL 和 Bash。以下命令在服务器的 root 终端运行。使用 sudo 登录的用户可先执行 `sudo -i`。 [安装 Docker Engine](https://docs.docker.com/engine/install/) · [安装 Compose 插件](https://docs.docker.com/compose/install/linux/)

### 创建首个邮箱

下载部署文件。

```bash
git clone --depth 1 https://github.com/makessr/HarborMail.git
cd HarborMail
```

复制下方整个代码块，输入一个至少 12 字节、最多 72 字节的邮箱密码。登录账号是 `hello@harbor.test`，自带管理员权限。数据库密码由命令随机生成。此步骤只在首次安装时运行。

```bash
(
  set -Eeuo pipefail
  umask 077
  for file in .env secrets/{accounts.json,mysql-password,mysql-root-password,mysql-dsn}; do
    if [[ -e "$file" || -L "$file" ]]; then
      printf '已有部署文件 %s，请保留现有配置。\n' "$file" >&2
      exit 1
    fi
  done
  set -o noclobber
  docker pull ghcr.io/makessr/harbormail:latest
  read -rsp '输入邮箱密码（12–72 字节）: ' mailbox_password
  printf '\n'
  hash=$(printf '%s\n' "$mailbox_password" | docker run --rm -i ghcr.io/makessr/harbormail:latest hash-password)
  unset mailbox_password
  mysql_password=$(openssl rand -hex 24)
  mysql_root_password=$(openssl rand -hex 24)
  install -d -m 0700 secrets
  printf '{"hello@harbor.test":"%s"}\n' "$hash" > secrets/accounts.json
  printf '%s' "$mysql_password" > secrets/mysql-password
  printf '%s' "$mysql_root_password" > secrets/mysql-root-password
  printf 'harbor:%s@tcp(mysql:3306)/harbor_mail' "$mysql_password" > secrets/mysql-dsn
  chgrp 10001 secrets/accounts.json secrets/mysql-dsn
  chmod 0640 secrets/accounts.json secrets/mysql-dsn
  cat > .env <<'EOF'
COMPOSE_FILE=compose.yaml:compose.mysql.yaml:compose.registry.yaml
HARBOR_ADMINS=hello@harbor.test
HARBOR_WEB_PORT=8080
HARBOR_SMTP_PORT=2525
EOF
)
```

### 启动 MySQL 与 Redis

在同一目录启动。Compose 从 `.env` 读取部署文件和端口。 [Compose 环境变量](https://docs.docker.com/compose/how-tos/environment-variables/envvars/#compose_file)

```bash
docker compose config --quiet && docker compose up -d --no-build --wait
docker compose ps
```

### 启动后验收

- `harbor`、`mysql` 和 `redis` 均为 `healthy`。
- 在服务器上打开 [网页邮箱](http://127.0.0.1:8080/) 或 [管理后台](http://127.0.0.1:8080/admin)，用 `hello@harbor.test` 和刚输入的密码登录。给自己发一封邮件，在收件箱确认收到。
- 如果只有远程服务器，在**自己电脑的终端**执行 `ssh -N -L 8080:127.0.0.1:8080 用户名@服务器地址`，保持该终端开启，再打开上面的链接。若改了服务器 Web 端口，SSH 命令中第二个 `8080` 也要改。

开通公网访问见 [Cloudflare 自动证书](#cloudflare-dns-01)和 [公网邮件收发](#接入真实域名)。

更换 MySQL 地址或账号前，先运行 [MySQL 预检](../ops/README.md#mysql-预检)。

### 配置宿主机端口

端口被占用时，修改 `.env` 中已有的端口值，再运行 `docker compose up -d --no-build --wait`。浏览器和客户端也改用新端口。部署失败可先看 `docker compose logs --tail=80 harbor mysql redis`。

| 用途 | `.env` 变量 | 默认宿主机端口 |
| --- | --- | --- |
| Web 邮箱及后台 | `HARBOR_WEB_PORT` | 8080 |
| SMTP 收信 | `HARBOR_SMTP_PORT` | 2525 |
| SMTP 隐式 TLS | `HARBOR_SMTPS_PORT` | 465 |
| SMTP STARTTLS 提交 | `HARBOR_SUBMISSION_PORT` | 587 |
| IMAPS | `HARBOR_IMAPS_PORT` | 993 |
| POP3S | `HARBOR_POP3S_PORT` | 995 |
| ACME HTTP-01 | `HARBOR_ACME_PORT` | 8081 |
| 运行指标 | `HARBOR_METRICS_PORT` | 9091 |

默认 Web 和 SMTP 仅绑定本机回环。邮件客户端、HTTP-01 验证和运行指标端口由对应的覆盖文件启用。

MySQL 的 `3306` 和 Redis 的 `6379` 只在 Compose 网络内使用。宿主机上已经有服务占用这两个端口，Harbor Mail 仍可正常启动。自行发布数据库或 Redis 端口时，需选用空闲的宿主机端口。

升级镜像时在项目目录运行 `docker compose pull harbor && docker compose up -d --no-build --wait`，沿用原有凭据和数据卷。

### SQLite 与可选组件

使用 SQLite 时，创建邮箱后运行 `docker compose -f compose.yaml up -d --build --wait`。Web 与 SMTP 默认只绑定主机回环地址，Harbor 容器以 UID/GID `10001:10001` 运行，SQLite 数据保存在 `harbor_data` 卷。

| 可选 Compose 文件 | 用途 |
| --- | --- |
| `compose.mysql.yaml` | 启动内部 MySQL 8.4 和专用 Redis，数据库保存在 `mysql_data` 卷 |
| `compose.registry.yaml` | 使用 GHCR 中的 Harbor Mail 镜像，关闭源码构建 |
| `compose.redis-cache.yaml` | 可选，用已有 Redis 替代随 MySQL 启动的缓存容器，也可用于 SQLite |
| `compose.tls.yaml` | 启用邮件客户端端口 465、587、993、995，默认映射到回环地址 |
| `compose.acme.yaml` | 使用 ACME 管理邮件 TLS 证书，并提供 HTTP-01 验证端点 |
| `compose.acme-cloudflare.yaml` | 通过 Cloudflare DNS-01 自动签发和续签证书 |
| `compose.relay-auth.yaml` | 从文件读取 SMTP 中继密码 |
| `compose.domain-relays.yaml` | 从文件配置各发件域的专用中继 |
| `compose.relay-ca.yaml` | 只读挂载 SMTP 中继的 CA 证书目录 |

使用三步部署的读者可在 `.env` 的 `COMPOSE_FILE` 末尾用冒号追加覆盖文件。使用 `-f` 的部署应在查看日志、重启、升级和停止时沿用同一组文件与项目名。

收到 `SIGTERM` 或 `Ctrl+C` 后，Harbor 停止监听和领取新任务，最多等待两分钟，让当前 Web 请求、SMTP 会话和已领取的外发任务收尾。已连接的 SMTP 客户端仍可完成当前事务，新事务会收到 `421`。IMAP、POP3 和网页 SSE 连接会断开，客户端可在服务回来后重连。Compose 为 Harbor 容器保留 125 秒的停止时间。

需要从源码构建时，省略 `compose.registry.yaml`，启动时使用 `--build`。构建主机可通过 `HARBOR_GO_PROXY` 和 `HARBOR_GO_SUMDB` 设置 Go 模块代理与校验服务。

- **TLS** 证书放在 `tls/fullchain.pem` 和 `tls/privkey.pem`，可用 `HARBOR_TLS_DIR` 指定其他目录。证书须覆盖客户端使用的邮件主机名，挂载目录和证书文件须能被容器 UID `10001` 读取。私钥应通过受限的属主或组权限提供。 [轮换 PEM 证书](#轮换-pem-证书)
- **中继认证** 在 `.env` 中填写 `HARBOR_RELAY` 和 `HARBOR_RELAY_USER`，把密码存入 `secrets/relay-password` 并加载对应 Compose 文件。默认使用 STARTTLS；465 模式通常设置 `HARBOR_RELAY_TLS=implicit`。
- **中继私有 CA** 把 CA 的 PEM 证书放入 `relay-ca/`，确保容器 UID `10001` 可读。使用 `-f compose.relay-ca.yaml`，并在 `.env` 设置 `HARBOR_RELAY_CA_FILE=/run/harbor-relay-ca/company.pem`。可用 `HARBOR_RELAY_CA_DIR` 覆盖宿主机目录。按域配置时，在该域的 JSON 中填写容器内的证书路径。更新证书后重启 Harbor 服务。

在 Linux 上请保护主机上的凭据目录，确认挂载到 Harbor 容器的文件可由 UID `10001` 读取。可用 `HARBOR_SECRETS_DIR` 指定主机上的私有目录。公开收信时按下方域名部署说明设置 MX，并配置 `HARBOR_SMTP_BIND=0.0.0.0`、`HARBOR_SMTP_PORT=25`。若要开放客户端 TLS 端口，设置 `HARBOR_TLS_BIND=0.0.0.0`。Web 容器端口提供 HTTP，由 HTTPS 反向代理接入时设置 `HARBOR_SECURE_COOKIE=true`。

重建容器会继续使用已有数据卷。日常停止服务可用 `docker compose … stop`，保留现有数据。只有清理演练环境时才使用 `down --volumes`，它会删除该项目的命名数据卷。SQLite 与 MySQL 数据卷各自独立，已有邮箱请按[迁移文档](#从-sqlite-迁移到-mysql)切换。

## 配置

| 环境变量 | 用途 |
| --- | --- |
| `HARBOR_ACCOUNTS` | 必填。JSON 对象，键为邮箱地址，值为 bcrypt 哈希 |
| `HARBOR_ACCOUNTS_FILE` | 从文件读取账号 JSON，与 `HARBOR_ACCOUNTS` 二选一 |
| `HARBOR_ADMINS` / `HARBOR_ADMINS_FILE` | 可选。逗号分隔的管理员主邮箱，留空时所有账号均为普通用户 |
| `HARBOR_ADMIN_MFA_FILE` | 可选。管理员 TOTP 配置 JSON，用 `compose.admin-mfa.yaml` 挂载 |
| `HARBOR_CONTENT_INDEX` | 可选。设置为 `true` 后启动时初始化主题和正文索引 |
| `HARBOR_ALIASES` | 可选，JSON 对象，键为收信别名，值为主邮箱 |
| `HARBOR_ALIASES_FILE` | 从文件读取别名 JSON，与 `HARBOR_ALIASES` 二选一 |
| `HARBOR_DOMAIN` | SMTP 服务名，默认 `harbor.test`，公开部署时设为邮件主机的 FQDN |
| `HARBOR_WEB` | 网页监听地址，默认 `127.0.0.1:8080` |
| `HARBOR_SMTP` | 收信监听地址，默认 `127.0.0.1:2525` |
| `HARBOR_RECOVERY_MODE` | 默认 `false`。设为 `true` 后仅开放 Web 校验，停止邮件协议与后台投递。见[恢复演练](../ops/README.md#恢复与校验) |
| `HARBOR_PAUSE_OUTBOX` | 默认 `false`。设为 `true` 后暂停处理发件队列，保留网页和已启用的邮件协议。共享数据库的所有实例均须设置 |
| `HARBOR_SUBMISSION` | 客户端发信监听地址，使用 STARTTLS，通常为 `:587`。默认关闭 |
| `HARBOR_SUBMISSIONS` | 客户端发信监听地址，使用隐式 TLS，通常为 `:465`。默认关闭 |
| `HARBOR_POP3S` | 客户端收信监听地址，使用隐式 TLS，通常为 `:995`。默认关闭 |
| `HARBOR_IMAPS` | 客户端 IMAP 监听地址，使用隐式 TLS，通常为 `:993`。默认关闭 |
| `HARBOR_DB_DRIVER` | 数据库驱动，默认 `sqlite`，也可设为 `mysql` |
| `HARBOR_DB` | SQLite 文件路径，默认 `data/harbor.db` |
| `HARBOR_MYSQL_DSN` | 使用 MySQL 时必填，格式见下方示例 |
| `HARBOR_MYSQL_DSN_FILE` | 从文件读取 MySQL DSN，与 `HARBOR_MYSQL_DSN` 二选一 |
| `HARBOR_REDIS_URL` / `HARBOR_REDIS_URL_FILE` | 可选。Redis 连接 URL，用于后台统计缓存，二选一 |
| `HARBOR_REDIS_NAMESPACE` | 启用 Redis 时设置。同一 Harbor Mail 部署的副本共用，独立部署使用不同值 |
| `HARBOR_CERT` / `HARBOR_KEY` | PEM 证书与私钥路径。启用 SMTP STARTTLS 和 Web HTTPS |
| `HARBOR_ACME_HOSTS` | 用逗号分隔的证书主机名，国际化域名填 Punycode。与 PEM 路径二选一 |
| `HARBOR_ACME_CHALLENGE` | `http`（默认，保留 HTTP-01 和 TLS-ALPN-01）或 `cloudflare`（仅 DNS-01） |
| `HARBOR_ACME_CLOUDFLARE_TOKEN` / `HARBOR_ACME_CLOUDFLARE_TOKEN_FILE` | Cloudflare DNS-01 的 API token 或其文件路径，二选一。推荐凭据文件 |
| `HARBOR_ACME_ACCEPT_TOS` | 阅读 CA 条款并同意后设为 `true`，开启 ACME 时必填 |
| `HARBOR_ACME_CACHE` | ACME 账号密钥、证书和私钥的持久目录。默认 `data/acme`，Compose 为 `/var/lib/harbormail/acme` |
| `HARBOR_ACME_HTTP` | HTTP-01 验证的监听地址，默认 `127.0.0.1:8081` |
| `HARBOR_ACME_EMAIL` | 可选的 ACME 账号联系邮箱 |
| `HARBOR_ACME_DIRECTORY` | 可选的 ACME HTTPS Directory URL，留空使用 Let's Encrypt 生产环境 |
| `HARBOR_WEB_TLS` | Compose 默认 `off`，由反向代理终结 Web TLS。启用 PEM 或 ACME 后设为 `on`，可由 Harbor 直接提供 HTTPS |
| `HARBOR_SECURE_COOKIE` | 反向代理终结 Web TLS 时设为 `true` |
| `HARBOR_LOGIN_AUDIT_DAYS` | 登录审计保留天数，默认 30，可设为 1–90。共享 MySQL 的实例使用同一值 |
| `HARBOR_TRUSTED_PROXIES` | 可选。以逗号分隔的可信反向代理 IP 或 CIDR，供 Web 登录限速识别来源 |
| `HARBOR_METRICS` | 可选的私有 Prometheus 指标监听地址，例如 `127.0.0.1:9091` |
| `HARBOR_RSPAMD_URL` | 可选，私有 Rspamd 扫描端点，例如 `http://127.0.0.1:11333/checkv2` |
| `HARBOR_RSPAMD_TRAIN_URL` | 可选，可信私有 Rspamd 控制器的 HTTP(S) 基础地址，例如 `http://127.0.0.1:11334`。留空关闭训练 |
| `HARBOR_RSPAMD_TRAIN_ACCOUNTS` | 开启训练时必填。用逗号分隔获准提交反馈的主账号地址 |
| `HARBOR_RSPAMD_TRAIN_PASSWORD` / `HARBOR_RSPAMD_TRAIN_PASSWORD_FILE` | 训练所用控制器权限密码，二选一。文件仅存放实际密码 |
| `HARBOR_RELAY` | 外部发信中继地址，如 `smtp.provider.example:587` |
| `HARBOR_RELAY_TLS` | `starttls`（默认，强制 STARTTLS）或 `implicit`（隐式 TLS，通常为 465） |
| `HARBOR_RELAY_USER` / `HARBOR_RELAY_PASSWORD` | 中继需要 SMTP AUTH PLAIN 时成对设置。配置缺项会导致启动失败 |
| `HARBOR_RELAY_PASSWORD_FILE` | 从文件读取中继密码，与 `HARBOR_RELAY_PASSWORD` 二选一 |
| `HARBOR_RELAY_CA_FILE` | 全局中继的附加 CA 证书 PEM 文件。单二进制部署使用服务可读的本机路径 |

## 管理后台

「存储用量」位于 `/admin/storage`，也可从账号列表进入。选定主邮箱后，按文件夹和投递状态查看原始邮件的数量与字节数。计算包括附件和实际保存的副本，队列按任务计数。统计按请求计算，不修改数据，也不包含数据库索引、重复文本字段、空闲页或备份，因此不能用来估算磁盘剩余空间或判断配额。

管理员可为每个主邮箱保存容量预警阈值（整数 MiB，最大 1 TiB，填 0 关闭）。用量以文件夹和投递队列保存的原文之和计算，达到 80% 和 100% 时在存储页显示状态。保存阈值时需验证管理员主邮箱密码，变更与审计同事务写入 SQLite 或 MySQL。预警随用量查询更新，收发仍按原有规则进行。

启用硬配额前先停止对同一数据库写入的所有实例，并备份。首次以 `HARBOR_HARD_QUOTA=true` 启动会安装数据触发器并从现有数据计算用量。MySQL 需要建触发器权限，使用二进制日志时还需符合服务器的触发器部署策略。激活保存在数据库中，所有实例须统一升级。启用后可在存储页单独设置硬配额（0 为无限制）。配额统计邮件和队列原文，拒绝使账号超限的新写入，已有邮件仍可读取或移动。要释放空间，可打开废纸篓中的邮件并确认「永久删除」。降低配额后，已接受的队列任务仍可归档发件副本，也可在允许取消时转回草稿。目标邮箱满额时站内投递失败。配置可独立于容量预警调整。

共享数据库的实例会依次检查硬配额状态。一个实例完成激活或修复后，后续实例沿用已计算的用量。若确认触发器缺失，请先暂停共享数据库的写入，再启动实例自动补齐触发器并重算用量。

公网 SMTP 收信会在 `RCPT TO` 阶段逐地址检查硬配额。邮箱已满或剩余空间不足时，对该地址返回 `452`，同封邮件中有空间的地址仍可继续接收。客户端声明的 `SIZE` 大于该邮箱总额度时，对该地址返回 `552`。同一邮箱的不同别名各计一份。最终写入仍按实际邮件大小检查配额。若在 `DATA` 阶段超额，整次写入回滚并返回 `452`，让上游有机会重试。 [SMTP SIZE 扩展](https://www.rfc-editor.org/rfc/rfc1870.html#section-6.4)

将已有主邮箱加入 `HARBOR_ADMINS`，例如 `HARBOR_ADMINS=admin@example.com,ops@example.com`，然后重启 Harbor Mail。Compose 部署在 `.env` 中设置同名变量并重新创建 Harbor 容器。原生部署也可通过 `HARBOR_ADMINS_FILE` 从文件读取名单。收信别名不能配置为管理员。

管理员登录后进入 `/admin`，可查看邮箱账号、角色、别名、服务配置和投递队列统计。选择「我的邮箱」可回到个人邮箱。普通用户登录后进入个人邮箱，后台页面和管理 API 均由服务端验证角色。账号、别名及管理员角色通过服务器配置文件或环境变量维护。

请在服务启动后，从浏览器访问实际 Web 地址的 `/admin`。本机默认是 `http://127.0.0.1:8080/admin`。源码中的 `internal/server/admin.html` 是嵌入式页面模板，必须通过 Harbor Mail 服务访问。

概览显示当前队列分布、最久到期等待、最近队列任务和近 7 天管理操作。管理操作按 UTC 自然日汇总。可按队列状态或任务编号下钻，管理操作明细在审计页。

「运行监控」位于 `/admin/runtime`。Windows 与 Linux 显示 Harbor Mail 进程 CPU、常驻内存和 Go 堆对象内存，另有数据库连接池、探测耗时、队列等待及训练失败数。CPU 以一个逻辑核心满载为 100%，多核可超过 100%。运行监控页打开且可见时每十秒采样一次，最多保留本页最近 30 个样本，可暂停。MySQL 和 Rspamd 进程需单独监控，长期采集用 `HARBOR_METRICS` 与主机监控。[Windows 工作集](https://learn.microsoft.com/en-us/windows/win32/procthread/process-working-set) · [Linux 进程指标](https://www.kernel.org/doc/html/v6.16/filesystems/proc.html) · [Go 运行时指标](https://pkg.go.dev/runtime/metrics)

全站管理员可在 `/admin/security` 查看 Web、SMTP 提交、IMAP 和 POP3 登录审计，按邮箱账号、来源、协议、日期和结果筛选，每页 100 条。SQLite 在本地持久化审计，共享 MySQL 的实例使用同一份记录。日期筛选按浏览器所在时区计算。Web 登录与邮件客户端分别计数，三个邮件协议共享客户端计数。每五分钟同一来源最多 30 次，同一来源向同一账号最多 8 次。Web 超限返回 HTTP 429 和 `Retry-After`，邮件客户端收到临时认证失败。IPv6 按 /64 聚合，邮件协议使用 TCP 对端地址。SQLite 部署使用进程内限速。共享 MySQL 的 Harbor Mail 实例共用限速窗口，数据库无法确认额度时暂缓认证。登录统计覆盖整个保留期。过期记录在查询时立即隐藏，服务启动及之后每小时清理。审计记录包含尝试登录的账号、来源地址、协议、时间和结果。升级后开始积累，审计随数据库备份和迁移，备份中的副本由备份保留策略管理。

为管理员启用网页登录第二因素时，在可信且不录屏、不记录输出的终端运行以下命令。把示例地址换成管理员邮箱。

```sh
docker run --rm -it --log-driver=none ghcr.io/makessr/harbormail:latest generate-admin-mfa admin@example.com --qr
```

二进制部署使用 `harbormail generate-admin-mfa admin@example.com --qr`。用支持 TOTP 的认证器扫描终端二维码。终端需支持 ANSI 色彩和 Unicode，必要时拉宽窗口避免换行。也可以省略 `--qr`，使用命令输出的 URI 或配置中的 `secret` 手动绑定。[认证器 URI 格式](https://github.com/google/google-authenticator/wiki/Key-Uri-Format)

将配置 JSON 存入权限受限的 `HARBOR_ADMIN_MFA_FILE`。使用 Compose 时，将文件保存为 `${HARBOR_SECRETS_DIR:-./secrets}/admin-mfa.json`，并在部署命令中叠加 `-f compose.admin-mfa.yaml`。多个管理员的配置合并到同一个 JSON 对象。共享数据库的实例用同一份配置并同步重启，然后用认证器验证码登录管理后台确认绑定。一次性恢复码须离线保存。二维码、URI 和配置 JSON 均含凭据，避免截图、写入日志或提交到仓库。

种子启用或轮换会使旧管理员网页会话失效。验证码和恢复码均在登录会话成功入库时一次性消耗。MySQL 部署使用数据库时钟核对 TOTP 有效期。认证器遗失时可用恢复码登录，再由服务器运维轮换种子。[TOTP 标准](https://www.rfc-editor.org/rfc/rfc6238.html)

默认使用直接连接的来源。Web 服务位于反向代理之后时，应让后端仅允许可信代理访问，把这些代理的实际来源 IP 写入 `HARBOR_TRUSTED_PROXIES`，并让代理覆盖或正确追加 `X-Forwarded-For`。共享代理或多个实例部署可同时在代理层实施统一限速。[可信代理选择规则](https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/X-Forwarded-For#selecting_an_ip_address)

升级共享 MySQL 的部署时，将所有实例更新至支持共享限速的版本。程序启动时会自动创建限速表。

「域名」位于 `/admin/domains`，显示后台登记的域名，以及由已有主邮箱和别名汇总出的域名。管理员可以先登记域名，再检查 DNS、查看关联账号或直接创建邮箱。新增和移除须重新验证管理员主邮箱密码，域名登记与审计随 SQLite 或 MySQL 保存。后台只允许移除没有主邮箱和别名的登记。新增域名后，公网收发仍需按[接入真实域名](#接入真实域名)完成 DNS 和中继配置。

可用 `HARBOR_DOMAIN_ADMINS` 或 `HARBOR_DOMAIN_ADMINS_FILE` 分配域管理员，例如 `{"operator@example.com":["team.example.com"]}`。账号须来自启动配置。域管理员可以在授权域创建普通邮箱、管理同时位于授权范围的别名和目标邮箱、检查 DNS。全站安全、投递队列、审计与管理员账号操作仍由全站管理员负责。可为域管理员同样配置 TOTP。

`HARBOR_DOMAIN_DEFAULT_QUOTAS`（或对应 `_FILE`）接收 JSON，例如 `{"team.example.com":2048}`。已启用硬配额时，新创建的该域邮箱继承此值，单位为 MiB。全站管理员仍可逐邮箱覆盖。更改默认值用于后续新建邮箱。

`HARBOR_DOMAIN_RELAYS_FILE` 可配置每个发件域的专用中继。JSON 的域名键对应 `address`（主机和端口）、`tls`（`starttls` 或 `implicit`）、`user` 与 `password`。认证字段同时留空表示仅使用 TLS。可选字段 `caFile` 为该域的中继添加私有 CA。未单独配置中继的域沿用全局中继及其 CA。Docker 部署可加载 `compose.domain-relays.yaml`，将文件存为秘密目录下的 `domain-relays.json`。各发件域可由中继执行 DKIM 签名，也可启用 [Harbor Mail 内置 DKIM](../ops/README.md#启用内置-dkim)。

```json
{
  "team.example.com": {
    "address": "smtp.internal.example:587",
    "tls": "starttls",
    "caFile": "/run/harbor-relay-ca/team.pem"
  }
}
```

私有 CA 与系统 CA 一起使用。Harbor Mail 在启动时读取证书文件，文件无法读取或没有有效 PEM 证书时会报错。STARTTLS 和隐式 TLS 都验证证书链及中继地址中的主机名。

上线前可运行 `harbormail relay-check hello@example.com`，验证选中的中继、TLS 和认证。追加 `--mail-from` 还可检查中继是否在信封阶段接受该地址，整个过程不会投递邮件。 [SMTP 中继预检](../ops/README.md#smtp-中继预检)

选中域名后可手动检查 MX、SPF 和该域的 DMARC 记录。已启用内置 DKIM 时，留空即可检查当前选择器。也可以手动检查其他选择器的公钥记录。填入中继提供的选择器或邮件 DKIM 签名中 `s=` 的值。结果以服务器 DNS 解析器看到的记录为准，实际投递和发信认证仍需联调。[DKIM 查询规则](https://www.rfc-editor.org/rfc/rfc6376.html#section-3.6.2.1) · [DMARC 标准](https://www.rfc-editor.org/info/rfc9989/)

服务启动前，也可从终端运行 [DNS 预检](../ops/README.md#邮件域名-dns-预检)，无需登录后台。

「投递队列」位于 `/admin/queue`，可按状态、发件账号和邮件时间筛选。时间筛选使用浏览器时区，截止日期当天不纳入。列表显示任务编号、尝试次数、下次处理时间和投递说明。成功投递会从队列移除。

有外发中继时，管理员可将仍在等待的临时失败任务提前到现在，由既有投递进程领取。永久失败、结果待确认和已领取的任务不支持这个操作。每次提前重试与审计记录同一事务保存，页面显示最近 20 条。管理员可见的是发件账号和投递元数据，邮件正文、主题、附件和收件地址不会通过此页面返回。

结果待确认的任务可在核对中继日志后结案。确认已接收会按正常投递结果归档到「已发送」，确认未接收会保留原邮件并标为失败。两种结案都要求重新验证管理员密码、记录审计，且不触发重发。

「账号」位于 `/admin/accounts`，可按域名、角色、主邮箱或别名查找账号。从「管理访问」检查有效网页登录会话与应用密码数量，按需撤销其中一类。执行前须重新输入管理员自己的主邮箱密码。撤销与审计记录同一事务提交。撤销自己的全部网页登录会话也会退出当前后台。

使用「新建账号」创建普通邮箱账号。提交完整地址、初始密码并重新验证管理员密码。账号存入 SQLite 或 MySQL，后续登录、投递和后台别名可立即使用。为防止旧邮箱数据被新账号继承，已有账号、别名和带历史邮箱数据的地址不可重复创建。管理员和启动账号继续由服务器配置提供。共享 MySQL 多实例升级时应同步更新所有实例及配置。

全站管理员可在「邮箱管理」迁移后台创建的账号，或回收、恢复和清除数据。迁移保留原始 MIME 和文件夹、规则等用户数据，旧地址变为收信别名。从 Web 邮箱发送迁移前保存的草稿时，待发送邮件使用新的主邮箱地址作为发件人。迁移和回收都会撤销网页会话及应用密码，待处理或结果待确认的投递须先结案。回收阻止新登录和收信，恢复还原回收前的登录状态。清除只允许用于已回收邮箱，重新验证管理员密码并输入原地址，历史主地址继续保留且不能分配给新账号。操作全部记入审计。

撤销应用密码影响后续客户端认证，已建立的 SMTP、IMAP 和 POP3 会话可以持续到断开。账号仍可使用主密码重新登录或生成应用密码。

「停用账号登录」阻止普通账号新建 Web、SMTP 提交、IMAP 和 POP3 登录，同时撤销其网页会话及应用密码。系统继续接收该账号与别名的来信，保留现有邮件及之前提交的投递任务。已建立的邮件协议连接可以持续到断开。恢复后可用原主密码重新登录，按需签发新的应用密码。

停用状态保存在 SQLite 或 MySQL，共享 MySQL 的实例应同步升级。后台不能停用当前具备管理员角色的账号，提升已停用的账号为管理员前应先恢复其登录。

管理员可通过「管理访问」为普通账号重置主邮箱密码，需重新验证管理员自己的密码。新密码对 Web、SMTP 提交、IMAP 和 POP3 的后续认证生效。旧网页会话和应用密码会与重置一起撤销；已建立的邮件客户端连接可持续到断开。操作记入审计，数据库只保存新密码的 bcrypt 哈希。管理员密码通过服务器配置维护。

后台重置绑定到当时的服务器账号哈希。以新的 `HARBOR_ACCOUNTS` 哈希替换账号配置时，以新配置为准。恢复旧配置和数据库备份前须核对密码变更及撤销状态。

「别名」位于 `/admin/aliases`。管理员可选择目标主邮箱，重新输入自己的主邮箱密码后添加或移除后台别名。变更保存在 SQLite 或 MySQL，并在后续投递时生效。账号页、域名页和个人邮箱「收信地址」都会读取生效的别名。新域需要配置其 MX。别名仅用于收信，登录和发信仍使用主邮箱。

服务器配置中的别名继续在后台显示，由原配置维护。主邮箱和服务器配置优先于同名后台别名。已入队的邮件保留原路由。开始投递前发现路由变化时，发件箱显示投递失败。共享 MySQL 的实例应同步升级并使用一致的账号和服务器别名配置。

「审计」位于 `/admin/audit`，集中查看账号停用与恢复、网页会话与应用密码撤销、收信别名变更、投递提前重试。管理员可按操作者、完整目标邮箱、操作类型和日期筛选。重试记录的目标账号是发件账号。页面时间使用浏览器时区，导出的 CSV 使用 UTC，每次最多导出 2000 条。审计包含操作元数据，不含邮件内容或访问凭据。

## 使用 MySQL

先创建数据库，并为 Harbor Mail 准备一个仅有该库 `CREATE`、`SELECT`、`INSERT`、`UPDATE`、`DELETE` 权限的账号。首次启动时自动创建邮件和登录会话表。MySQL 8.4 已验证。

```powershell
$env:HARBOR_DB_DRIVER = 'mysql'
$env:HARBOR_MYSQL_DSN = Read-Host '输入 MySQL DSN' -MaskInput
.\harbormail.exe
```

本机 DSN 示例为 `harbor:your-password@tcp(127.0.0.1:3306)/harbor_mail`。远程 MySQL 建议使用 `?tls=true`，并让证书匹配 DSN 中的主机名。若需要接收接近 10 MiB 的邮件，请为 MySQL 的 `max_allowed_packet` 保留至少 16 MiB，并检查数据库代理是否有更小的限制。

SQLite 和 MySQL 分别存储邮件和会话。切换驱动后会读取目标数据库。将既有 SQLite 安装迁往 MySQL 时，先按[迁移流程](#从-sqlite-迁移到-mysql)导入。MySQL 库可用 `mysqldump --single-transaction` 备份。

### 可选 Redis 缓存

Docker 的 MySQL 模式已包含 Redis。如果使用已有 Redis，将 `compose.redis-cache.yaml` 放在 MySQL 等其他覆盖文件之后，并把 URL 放在秘密目录的 `admin-redis-url` 文件中，设置唯一的 `HARBOR_REDIS_NAMESPACE`。从内置 Redis 切换前，可用原 Compose 文件组合执行 `stop redis` 停止旧缓存容器。原生部署可设置 `HARBOR_REDIS_URL`（例如同机 Redis 的 `redis://127.0.0.1:6379/0`）及命名空间。远程 Redis 使用带证书验证的 `rediss://` 并通过 `HARBOR_REDIS_URL_FILE` 提供连接凭据。

后台概览缓存 5 秒，邮箱存储明细缓存 10 秒。存储阈值和实际启用的硬配额仍实时读取数据库。Redis 超时或不可用时直接查询数据库，邮件、登录权限、发件箱状态和配额执法继续由原数据库处理。Redis 访问应限制在 Harbor Mail 和受信任的运维账户。[Redis URL 配置](https://redis.io/docs/latest/develop/clients/connection-urls/)

### 从 SQLite 迁移到 MySQL

1. 停止源和目标 Harbor Mail，核对正在投递的队列任务并生成一致的 SQLite 快照。保留现有账号配置和其他部署凭据。
2. 建立独立、空的 MySQL 数据库。将目标 DSN 通过 `HARBOR_MIGRATION_MYSQL_DSN_FILE` 或 `HARBOR_MIGRATION_MYSQL_DSN` 提供。执行 `harbormail migrate-sqlite --sqlite /path/to/harbor.db` 做只读预检。设置目标 DSN 时，预检还会检查已有目标表是否具备待迁移的列。
3. 在相同命令上加 `--apply`。源库有正在投递的任务、还在等待 SMTP 最终确认的收件人，或目标库已有数据时，程序会拒绝迁移。迁入后的记录按主键顺序核对 SHA-256，再提交 MySQL 事务。初始化完成的空表可供失败后重试。连接在提交时中断，应先核对目标有无已提交的数据。
4. 先以隔离监听地址和 `HARBOR_PAUSE_OUTBOX=true` 启动目标，验证邮箱、附件、文件夹和队列，按中继记录核对待确认的投递。确认切换后再恢复投递。保留 SQLite 和既有配置供回退。目标处理新邮件后，回退前须协调新邮件和队列的去向。 [恢复与回滚](../ops/README.md#切换与回滚)

## 接入邮件客户端

配置 TLS（PEM 文件或下述 ACME 模式），并启用一个或两个客户端发信端口。端口 587 选择 **STARTTLS**，端口 465 选择 **SSL/TLS**。服务器填写证书匹配的主机名，用户名填写完整邮箱地址，使用登录 Web 邮箱的密码和客户端的「普通密码」认证（SMTP AUTH PLAIN，始终在 TLS 内）。

客户端发件地址必须与登录账号一致。每次 SMTP 提交支持最多 20 个收件人。对外发信优先使用发件域的专用中继，未设置专用中继时使用 `HARBOR_RELAY`。站内直接投递，发件副本显示在 Web 邮箱的「已发送」。公网部署时，可在防火墙或 TCP 代理限制异常连接。

如需客户端收信，设置 `HARBOR_POP3S=:995` 并保留相同的证书配置。收信协议选择 **POP3**，连接安全性选择 **SSL/TLS**，端口 **995**，用完整邮箱地址和 Web 邮箱密码登录。POP3 下载的是收件箱。若客户端设置为下载后删除，只有在客户端正常发送 `QUIT` 后才会把邮件移到 Web 邮箱的废纸篓，可从那里恢复。需要保留收件箱中的原件时，启用客户端的「在服务器上保留副本」。

需要按文件夹浏览时，设置 `HARBOR_IMAPS=:993`，在客户端选择 **IMAP**、**SSL/TLS**、端口 **993**，使用完整邮箱地址和 Web 邮箱密码。客户端可以在收件箱、已发送、归档和废纸篓中读取、搜索、下载、移动和复制邮件，已读、未读和星标与 Web 邮箱共用。支持 `IDLE` 的客户端会收到当前选中文件夹的新邮件、移除和标记变更。重新选择文件夹或轮询（`NOOP`）也会读取其他会话的变更。

共享 MySQL 的部署每五秒检查跨实例变更。正在选中的自定义目录被另一会话删除时，服务端会在 IDLE 中或下一次进入 IDLE 时发送 `BYE` 并关闭连接，客户端可重连后选择有效目录。 [IMAP IDLE 协议](https://www.rfc-editor.org/rfc/rfc2177.html)

同一进程内，已提交的邮箱变更会唤醒对应账号的所有 IDLE 会话。多个 Harbor Mail 实例共享 MySQL 时，IDLE 约每 5 秒检查该账号的变更版本，发现变化后重新读取选中文件夹。共享库的各实例应使用包含此功能的版本。SQLite 支持单实例事件推送，部署时应使用独占数据卷。IDLE 推送的是当前快照之间的变化，直接修改数据库及旧版本写入不提供变更信号。

空闲连接每 5 分钟发一次状态响应。客户端宜按 [RFC 2177](https://www.rfc-editor.org/rfc/rfc2177.html#section-3) 至少每 29 分钟结束并重新发出 IDLE。重连后重新选中文件夹即可获得最新状态；改名后的文件夹沿用原来的 UID，已删除的选中文件夹会结束其 IDLE 连接。

网页通过 `/api/events` 的 SSE 长连接刷新当前邮件列表和文件夹导航，重连后会重新读取。反向代理应对该路径关闭响应缓冲，并允许间隔 15 秒的心跳通过。同一进程处理的变更会立即通知网页。多个实例共用 MySQL 时，网页连接约每 5 秒检查一次当前账号的邮箱修订号，检测到新邮件、标记、文件夹或发件箱状态等变更后自动刷新。发件箱的「准备投递」和「投递中」也会推进修订号。共享数据库的实例应使用支持修订号的版本。

支持 `COPY`、`MOVE` 及其 UID 形式。传统客户端标记删除（`\Deleted`）后，执行 `EXPUNGE` 或在可写会话中执行 `CLOSE` 会把邮件移入废纸篓。可以在 Web 邮箱恢复，或通过 IMAP 移回目标文件夹。如需永久删除，请在 Web 邮箱的废纸篓打开该邮件并确认「永久删除」。升级会自动补建 IMAP 元数据表，原有邮件无需导入。

## 邮件客户端的应用密码

在网页账号区选择「应用密码」，为客户端填写名称、勾选 IMAP、POP3 或 SMTP 提交，并选择有效期。创建时需再次输入主邮箱密码。新密码只显示一次，客户端的用户名仍填写完整主邮箱地址。

列表显示各凭据的授权协议、有效期和最近一次成功登录时间。撤销立即影响新的客户端登录，已建立的连接持续到断开。替换服务端配置中的主邮箱密码哈希后，之前创建的应用密码会失效。网页登录继续使用主邮箱密码。

## 网页登录设备

在网页账号区打开「登录设备」，可以查阅当前会话和同一账号的其他网页登录。新登录会记录浏览器及操作系统的简要标签、登录时间、最近活动和到期时间。升级前的有效网页登录会继续列出。

可以退出单个其他浏览器，也可以一次退出所有其他设备并保留当前网页登录。被退出的浏览器下次请求会要求重新登录。同一服务进程中，已连接的网页也会收到退出通知。本功能管理网页登录，客户端应用密码在「应用密码」页面单独撤销。

## 接入真实域名

1. 将邮件主机名解析到收信服务器，将每个收信域名的 MX 指向该主机，并把公网 TCP 25 转发到 Harbor Mail 的 SMTP 端口。
2. 给 Web 界面配 HTTPS。使用反向代理时保持 Web 监听在内部地址，代理端配置登录接口限流，后端开启 Secure Cookie。
3. 配置支持发件域名的 SMTP 中继，按中继要求配置 SPF、DKIM 和 DMARC。外部邮件先入发件箱，再由中继投递。
4. 备份 SQLite 文件时使用 SQLite 在线备份工具，或停服后备份整个数据库目录。证书与账号配置另行备份。

## 轮换 PEM 证书

外部续签工具可将新证书和私钥分别写入同一文件系统的临时文件，设置访问权限，再重命名覆盖 `HARBOR_CERT` 和 `HARBOR_KEY` 指向的文件。运行中的 Harbor 每五秒检查证书对。两个文件都就绪后，新 TLS 握手使用更新后的证书，已有连接继续运行。若文件缺失、私钥不匹配或新叶证书超出有效期，Harbor 保留当前证书，并继续重试。

Docker 下保留 `HARBOR_TLS_DIR` 对应的挂载目录，更新目录内的文件。若使用符号链接，请确保目标也在容器可访问的路径内。原生 HTTPS 的容器健康检查会核对当前服务端证书与 PEM 文件，两个文件尚未就绪时可能暂时失败。可从服务日志确认 `Reloaded TLS certificate`，再从外部入口核对实际生效的证书。

## 自动签发与续签 TLS 证书

### Cloudflare DNS-01

先完成[三步 Docker 部署](#docker-compose-部署)。域名的权威 DNS 需由 Cloudflare 托管。Let’s Encrypt 通过临时 TXT 记录验证域名，无需开放公网 80 或 443。 [ACME DNS-01](https://letsencrypt.org/docs/challenge-types/#dns-01-challenge)

1. 在 Cloudflare 创建 API token，授予目标域名 **Zone → DNS → Edit** 和 **Zone → Zone → Read**。将 token 保存到 `secrets/acme-cloudflare-token`。曾设置 `HARBOR_SECRETS_DIR` 的部署使用该目录。 [创建 Cloudflare API token](https://developers.cloudflare.com/fundamentals/api/get-started/create-token/)
2. 在项目目录给 token 文件设置权限。用了自定义凭据目录时，把下面命令中的 `secrets/` 换成该目录。

```bash
chgrp 10001 secrets/acme-cloudflare-token
chmod 0640 secrets/acme-cloudflare-token
```

3. 修改 `.env` 中的 `COMPOSE_FILE`，加入 Cloudflare 配置。将示例主机名改为自己的域名，阅读并同意 CA 条款后设置 `HARBOR_ACME_ACCEPT_TOS=true`。

```dotenv
COMPOSE_FILE=compose.yaml:compose.mysql.yaml:compose.registry.yaml:compose.acme-cloudflare.yaml
HARBOR_ACME_HOSTS=mail.example.com
HARBOR_ACME_ACCEPT_TOS=true
```

如需直接通过 `https://mail.example.com:8443/` 访问，在 Cloudflare 为该主机添加指向服务器的 **仅 DNS** 记录，在 `.env` 设置 `HARBOR_WEB_TLS=on` 和 `HARBOR_WEB_BIND=0.0.0.0`，将已有的 `HARBOR_WEB_PORT` 改为 `8443`，并在服务器防火墙及云安全组放行 TCP 8443。

```bash
docker compose config --quiet && docker compose up -d --no-build
docker compose logs -f harbor
```

启动前可用 `docker compose run --rm --no-deps -T harbor acme-check` 检查 Cloudflare 区域和 token 的读取权限。单二进制使用同一套环境配置运行 `harbormail acme-check`。预检只读，DNS 写入权限在实际签发时验证。

首次签发成功后再打开 HTTPS 地址。按 `Ctrl+C` 退出日志查看。Harbor 自动清理 TXT 记录并在 `HARBOR_ACME_CACHE` 下的 `cloudflare/` 保存证书及 ACME 账号状态。请保留该目录，并允许 Harbor 出站访问 CA、Cloudflare API 和 DNS。

单二进制部署需使用包含 Cloudflare DNS-01 的构建。设置 `HARBOR_ACME_CHALLENGE=cloudflare`，并将 `HARBOR_ACME_CLOUDFLARE_TOKEN_FILE` 指向服务账号可读的文件。systemd 可用 `LoadCredential=` 传入 token，再将文件路径配置为相应的 `%d/` 路径。 [下载构建产物](https://github.com/makessr/HarborMail/actions/workflows/publish-image.yaml)

### HTTP-01 与 TLS-ALPN-01

1. 将域名下的 A 和 AAAA 记录指向能够接收 CA 验证的入口。在 `HARBOR_ACME_HOSTS` 填写邮件和原生 HTTPS 服务实际使用的主机名，例如 `mail.example.com`。不发送 SNI 的邮件客户端会收到列表中第一个主机的证书。
2. 阅读发证机构的条款，在同意后设置 `HARBOR_ACME_ACCEPT_TOS=true`。将缓存放在进程可写、仅运行账号可访问的持久目录中。为生产 CA 和测试 CA 使用各自的缓存目录。每组域名由一个 Harbor 进程负责签发。
3. 使公网 TCP 80 可将 `/.well-known/acme-challenge/` 请求转发到 `HARBOR_ACME_HTTP`，保留原始 Host。Harbor 原生 Web HTTPS 暴露于公网 TCP 443 时，也可接收 TLS-ALPN-01 验证。保持验证入口可达，以便自动续签。[ACME 验证方式](https://letsencrypt.org/docs/challenge-types/)

Compose 选择 `compose.acme.yaml`，与 `compose.tls.yaml` 二选一。ACME 配置项写入 `.env`，并按需加载 MySQL 等 Compose 文件。

```text
docker compose -f compose.yaml -f compose.acme.yaml up -d --build --wait
```

默认验证请求由主机上的反向代理转发到 `127.0.0.1:8081`。代理应优先转发挑战路径，再执行其他 HTTP 跳转规则。将 Web 邮箱经 HTTPS 反向代理接入时，开启 `HARBOR_SECURE_COOKIE=true`，并为代理的 Web HTTPS 入口配置证书。如果直接公开挑战端口，可设置 `HARBOR_ACME_BIND=0.0.0.0` 和 `HARBOR_ACME_PORT=80`。非容器部署直接设置 `HARBOR_ACME_HTTP`，认证请求始终从公网 80 或 443 进入。

由 Harbor 直接提供 Web HTTPS 时，在 Compose 的 `.env` 设置 `HARBOR_WEB_TLS=on`。容器通过回环地址探测 HTTPS 和数据库可用性。ACME 模式使用 `HARBOR_ACME_HOSTS` 的第一个主机名校验证书。使用私有 CA 时，把 CA 根证书加入容器的信任库。PEM 模式核对服务端证书与 `HARBOR_CERT` 配置的叶证书一致。

Harbor 在启动时检查缓存目录能否写入。出现 `HARBOR_ACME_CACHE is not writable` 时，检查目录权限与挂载的读写状态。首次签发成功后客户端即可建立受信任的 TLS 连接。续签成功后，新建连接使用新证书，无需重启 Harbor。容器重建时保留 `harbor_data` 卷。 [Go autocert](https://pkg.go.dev/golang.org/x/crypto%40v0.59.0/acme/autocert)

CI 使用本地 Pebble 测试 CA 验证 HTTP-01、TLS-ALPN-01、证书续签和缓存重启。接入真实域名前，使用 CA 的测试环境和独立缓存核对公网挑战路径、实际接入节点提供的证书，以及续签后的证书有效期。 [Pebble 测试 CA](https://github.com/letsencrypt/pebble/tree/v2.10.3)

## 邮箱偏好

在网页左下角打开「邮箱偏好」。可以选择浅色、深色或跟随当前设备系统主题，并在舒适与紧凑列表间切换。紧凑列表隐藏邮件正文摘要，保留发件人、主题、日期和发件箱状态。「打开邮件时自动标为已读」默认开启，可关闭并通过阅读工具栏手动标记。

主题与密度在保存前即时预览。保存后的偏好随账号同步，重新登录或换浏览器可以恢复。网页偏好只控制 Harbor Mail 网页界面与网页打开邮件时的行为。

## 快捷操作

点击邮件列表右上角的闪电图标，或在网页内按 `Ctrl+K`（macOS 为 `⌘K`）打开面板。在手机阅读邮件时，闪电图标也在邮件工具栏内。输入文件夹、标签、已保存搜索或操作的名称，使用方向键选择，按 Enter 执行。输入邮件关键词后可选择在「全部邮件」中搜索。

焦点在邮件列表内时，使用上下方向键移动，Home 或 End 跳到已加载列表的首行或末行，Enter 打开邮件。阅读邮件时按 Esc 可回到列表。快捷操作键在输入框中和其他弹窗打开时让位于输入与弹窗操作。

## 自定义文件夹

在 Web 邮箱侧栏选择「新建文件夹」，打开该文件夹后用标题旁的「管理文件夹」重命名或删除。阅读邮件时，用「移动到文件夹」选择目标。自定义文件夹按账号隔离，支持中文名称和空格，同一账号内名称忽略大小写。

可用 `/` 创建层级路径，例如 `项目/2026` 或 `INBOX/客户`。完整路径最多 64 个字符。缺少的父文件夹会自动创建，网页侧栏和移动菜单按完整路径显示。已有单层自定义文件夹可直接继续使用。

IMAP 客户端支持 `CREATE`、`RENAME`、`DELETE`、`SUBSCRIBE`、`UNSUBSCRIBE`、`COPY` 和 `MOVE`。列表以 `/` 分隔层级，`LIST` 标明子节点，`LSUB` 的单层通配查询可显示未订阅的上级路径。显式创建的文件夹默认订阅，自动创建的父文件夹可单独订阅。协议细节见 [RFC 3501](https://www.rfc-editor.org/rfc/rfc3501.html#section-6.3.8)。

重命名父文件夹会一起更新所有下级路径，保留其邮件 UID、订阅状态、邮件规则和保存搜索引用。系统文件夹不可改名或删除。删除自定义文件夹前须先处理其子文件夹、邮件和现有规则引用。若废纸篓中的邮件原属已删除的文件夹，恢复时会回到其原始系统文件夹。

## 邮箱别名

部署者可为同一个主邮箱分配多个收信地址。例如，设置 `HARBOR_ALIASES` 为 `{"orders@harbor.test":"hello@harbor.test","support@harbor.test":"hello@harbor.test"}`，并在 `HARBOR_ACCOUNTS` 配置 `hello@harbor.test`。Compose 部署可通过同名环境变量传入 JSON。服务启动时读取配置，地址须使用小写，目标必须是已配置的主邮箱。若使用其他域名的别名，该域名的来信也需要指向此服务。

网页账号区的「收信地址」会显示你自己的主邮箱和别名。SMTP 来信、站内网页投递、草稿发送和客户端 SMTP 提交都支持向别名发送。邮件存到主邮箱，保留信封收件地址以便搜索和设置规则。同一封来信发给多个不同地址时，每个地址分别投递，即使它们属于同一主邮箱。登录、SMTP 认证和发件都使用主邮箱。

## 联系人通讯录

从网页账号区打开「联系人」，可以按姓名或邮箱查找、新建、编辑或删除联系人，并保存备注。「写信」会带入邮箱地址。在邮件阅读页点击「添加发件人」，可以预填原邮件的发件地址和姓名。若该地址已保存，将打开现有联系人供编辑。

写邮件时，在「收件人」中输入姓名或部分邮箱即可选择联系人。支持鼠标或触屏点击，也可用方向键和回车选择。通讯录按账号隔离，同一账号下的邮箱地址忽略大小写去重，可直接输入任意有效的收件地址。

## 邮件规则

登录网页后，在账号区域打开「邮件规则」。每个邮箱最多可建立 50 条规则，规则可编辑、停用或删除。优先级可设为 1–1000，按数字从小到大检查，同级按创建顺序，只执行第一条匹配的启用规则。

- 发件地址和域名取自 SMTP 信封，支持精确匹配。域名规则匹配完整域名。
- 收件地址规则匹配实际投递地址，可以针对别名单独归档或分类。
- 主题对解码后的文字做包含匹配。三种条件均忽略大小写。
- 动作支持移动到「收件箱」「归档」「垃圾邮件」或自定义文件夹、添加标签、加星标或标为已读。添加标签适用于新投递邮件，并保留其文件夹。删除被规则引用的文件夹或标签前，需修改或删除相关规则。

规则在新邮件投递时按收件人执行，适用于 SMTP 来信、站内投递和从草稿发送。已存邮件和系统退信各自保留原有处理方式。过滤后，邮件及状态会同步到对应的 Web 和 IMAP 文件夹。

## 搜索和批量整理

选择「全部邮件」可查看收件箱、已发送、归档及自定义文件夹的邮件。草稿箱、发件箱、垃圾邮件和废纸篓可在各自页面搜索。关键字可与发件人、收件地址、阅读状态及日期范围组合。日期按浏览器本地时区计算，包含开始日和结束日。发件箱支持关键字搜索。

在「筛选」中把关键词范围设为「仅主题和正文」可搜索邮件内容，并可保存该条件。维护时可用 `HARBOR_CONTENT_INDEX=true` 启用持久化索引并补建历史邮件。SQLite 使用 FTS5 trigram，适用于至少三个字符的关键词。MySQL 使用 ngram 索引加速达到服务器分词长度的连续汉字查询，建议保留默认分词长度与默认英文停用词配置。其他关键词仍走原有精确匹配。索引首建需要额外磁盘、索引写权限和维护窗口。大 MySQL 邮箱建立索引时可提高 DSN 的 `readTimeout`，迁移完成后在目标启动时重建索引。[SQLite FTS5](https://sqlite.org/fts5.html#the_trigram_tokenizer) · [MySQL ngram](https://dev.mysql.com/doc/refman/8.4/en/fulltext-search-ngram.html)

在邮件列表选择「批量整理」，点击邮件行进行多选，或选择已加载的结果。每次可处理最多 100 封，支持已读或未读、加星或取消星标、归档和移动到指定文件夹。批量操作在同一事务中执行，并同步到 Web 和 IMAP。

### 稍后处理

打开收件箱中的邮件，点击「稍后再看」，可选择 1 小时后、明早 9 点或设备时区中的自定义时间。邮件会移到网页侧栏的「稍后处理」，到时间后自动回到收件箱。可从「管理稍后处理」改时间或立即移回。到期任务保存在数据库，服务重启后会补处理。

等待期间，「全部邮件」、星标和标签仍可找到原邮件。IMAP 和 POP3 的收件箱也会暂时移开这封邮件，定时管理入口在网页。自动移回保留当时的已读和星标状态。主动把邮件移到其他文件夹会取消预约。

### 保存搜索

输入关键词或应用筛选后，点击「保存搜索」，为当前条件命名并选择范围。支持当前文件夹、全部邮件或标签。发件箱支持保存关键词搜索。点击侧栏中的搜索可重新查询最新结果，日期按当前设备时区计算。

打开某条已保存搜索后，可先调整搜索框或筛选条件，再通过「编辑搜索」更新条件、改名、改范围或删除。原范围被删除时，点击侧栏里的搜索可重新指定范围。删除搜索只会移除侧栏快捷入口。

### 邮件标签

在侧栏选择「管理标签」可新建、重命名或删除标签。打开一封邮件后，点击主题下方的「添加标签」或「编辑标签」，勾选需要的标签。每封邮件可以归入多个标签，同时保留当前文件夹。删除标签会清除该标签的关联，关联邮件仍保留。

点击侧栏标签后，可以在该标签范围内搜索并继续使用邮件筛选。废纸篓和垃圾邮件中的邮件保留原有标签，恢复到普通文件夹后会重新出现在标签视图。标签按账号保存在 SQLite 或 MySQL 中，在 Web 邮箱使用。EML 和 ZIP 导出保存邮件原件，标签属于数据库里的整理信息。

要整理已有邮件，选择「批量整理」并勾选邮件，从操作菜单选择添加或移除某个标签。每批最多 100 封，全部变更在同一事务内提交。要自动整理新来信，在「邮件规则」选择「添加标签」并指定目标标签。规则按现有优先级执行，仍以首条匹配的启用规则为准。

## 垃圾邮件

网页可在阅读工具栏标记垃圾邮件，也可以在「垃圾邮件」中选择「非垃圾邮件，移回收件箱」。IMAP 将系统目录公布为带 `\Junk` 属性的 `Junk`，客户端可用 MOVE 在文件夹之间移动。垃圾邮件保留原文和标记，从收件箱和星标列表中隔离。升级时，如已有同名自定义目录，Harbor Mail 会给旧目录名称加上「自定义」后缀，保留其中邮件、规则引用及 IMAP UID。

部署者可设置 `HARBOR_RSPAMD_URL`，让 Harbor Mail 将公共 SMTP 入口的原始邮件和信封信息发送给可信的 [Rspamd 扫描 worker](https://docs.rspamd.com/developers/protocol/)。变量留空时关闭扫描，仍可手动标记和使用邮件规则。在 Compose 部署中，使用容器可以访问的私有 worker 地址。

扫描返回 `add header`、`rewrite subject`、`reject`、`quarantine` 或 `discard` 时，邮件默认进入垃圾邮件目录，保留邮件原文。匹配到收件人自己的移动规则时，以规则目标为准，可以指定「收件箱」处理误判。扫描超时或不可用时继续按收件人的邮件规则投递。

### 分类训练

训练默认关闭。启用后，只有 `HARBOR_RSPAMD_TRAIN_ACCOUNTS` 中的主账号可以通过网页的「标记为垃圾邮件」和「非垃圾邮件，移回收件箱」提交反馈，支持单封与批量操作。训练处理实际移动且归属该账号的收件邮件。规则、普通移动和 IMAP MOVE 不提交训练。使用共享分类器时，请仅授权受信任的账号，并在共享同一数据库的实例上使用一致的设置。

训练会将原始邮件提交给可信的 [Rspamd 控制器](https://docs.rspamd.com/developers/protocol/)，包括邮件中的附件。控制器应仅供私有网络访问，跨不可信链路使用 HTTPS。配置控制器的 `enable_password` 时，Harbor Mail 需要对应的实际密码。建议启用 Rspamd 的训练缓存以识别重复训练。Compose 部署时把密码保存到 `${HARBOR_SECRETS_DIR}/rspamd-train-password`（默认 `./secrets/`），设置控制器地址和账号后，在现有 Compose 命令中加入 `-f compose.rspamd-learning.yaml`。

反馈与邮件移动同事务入库，控制器请求在后台进行。连接中断、HTTP 408、429 或 5xx 最多尝试五次，默认重试间隔依次为 20、40、80、160 秒。可重试响应包含有效的 `Retry-After` 时，使用同一数据库和控制器地址的实例会暂停领取该控制器的新样本，单次最多等一小时。等待时间保存在数据库中，进程重启后继续遵守，已经发出的请求会继续完成。单封反馈仍按自身退避和控制器等待时间中较长者重试。控制器明确拒绝或重试耗尽时标记失败，邮件继续保留在用户选择的文件夹。维护者可查看 `spam_feedback.state`、`attempts` 和 Harbor Mail 日志。日志记录反馈编号和错误类别。重新作出符合条件的分类操作可以再次提交失败的反馈。

## 邮件会话与回复

阅读邮件时可以展开「同一会话」，打开当前账号在收件箱、已发送、归档和自定义文件夹中的关联邮件。会话根据 `Message-ID`、`References` 和 `In-Reply-To` 关联，最多显示 100 封。已有邮件会在服务启动后补建索引，历史数据量较大时可稍后重新打开会话。

网页回复优先使用原邮件的 `Reply-To`，并生成 `In-Reply-To` 和 `References` 供邮件客户端识别会话。回复的引用关系会随草稿保存，支持继续编辑和带附件发送。[邮件标识字段说明](https://www.rfc-editor.org/rfc/rfc5322.html#section-3.6.4)

## 邮件转发

打开邮件后选择「转发」。「引用原文」会带入原邮件的发件人、收件人、时间、主题和纯文本正文，可同时带上原附件。作为 `.eml` 附件转发时，附件保留完整的原始邮件，适合仅有 HTML 正文或需要保留内嵌图片的邮件。两种方式都可以填写收件人和附言，编辑附件后直接发送或保存草稿。

转发沿用网页发信的附件上限，每封最多 10 个附件，合计 6 MiB。原附件超限时，可取消勾选「附带原邮件中的附件」，再手动选择要发的文件。完整 `.eml` 超限时，仍可从阅读区下载原件。

## 邮件签名

在账号区打开「邮件签名」，可以保存多份纯文本签名，分别为新邮件、回复和转发设置默认值。写信时可在「签名」菜单改用其他签名或取消自动插入。转发引用原文时，签名放在引用的上方。

签名插入后属于邮件正文，保存草稿时一并保存。继续编辑草稿会保留原正文，手动修改后的签名也会保留。签名设置按账号保存在 SQLite 或 MySQL 中，适用于网页写信。

## 邮件模板

写邮件时点击正文旁的「邮件模板」，可新建、预览、编辑或删除常用模板。模板包含名称、可选主题和纯文本正文，按账号保存。每个邮箱最多保存 20 份模板，单份正文最多 4 KiB。

新邮件、回复、转发和网页草稿都可以插入模板。正文插在光标处，有选区时插在选区末尾并保留原文字。模板主题只填入空白主题。插入的内容跟随当前邮件保存或发送，之后编辑模板也不会修改已插入的邮件。

## 网页发信的收件、抄送与密送

在写信窗口的「收件人」「抄送」或「密送」填写邮箱地址，支持用逗号或分号分隔。三栏合计最多 20 个去重地址，可以只填抄送或密送。同一个信封地址以收件、抄送、密送的顺序去重。

密送地址只用于确定投递目标。发件人可在自己的网页草稿、发件箱和已发送邮件中看到密送名单。投递的邮件头、下载的 `.eml` 和转发的原始邮件不包含该名单。正文转发引用原邮件头中的收件人和抄送，站内副本仍保留各自的实际投递地址供收信规则使用。 [RFC 5322 §3.6.3](https://www.rfc-editor.org/rfc/rfc5322.html#section-3.6.3)

含外部地址的邮件进入发件箱，需要配置 SMTP 中继。混合投递先提交站内副本。如果入队后移除了中继，站内副本仍会在发送时间到达后投递，外部地址继续在队列中等待原定投递期限内恢复中继。外部重试不重复投递站内副本。多个外部地址分别使用 SMTP 事务和持久化状态。中继拒绝一个地址时，其他地址继续投递，已成功的地址不会因临时失败而重投。中继最终确认丢失时只冻结当次地址。发件人可在发件箱查看各外部地址的结果，并在核对中继记录后单独结案。共享 MySQL 的部署应在停止所有旧版实例后升级，再恢复接入。

## 草稿箱

写信时点击「保存草稿」，收件人和正文可以稍后补填。草稿箱中的网页草稿可继续编辑，重新打开会加载已保存的附件。另一处先保存过时，旧版本提交会被拒绝，编辑内容仍留在当前窗口。通过网页发送后，原草稿进入可恢复的废纸篓；站内投递或外部入队与草稿移出在同一事务内完成。

IMAP 的 **Drafts** 文件夹支持 `APPEND`，原始 MIME 和附件会完整保存。客户端上传的草稿可在 Web 阅读或下载，继续编辑请使用邮件客户端。网页创建的草稿可以在 IMAP 读取，网页保存新版后旧 IMAP UID 会失效，客户端刷新即可看到新版。

### 自动保存

网页写信、回复、转发或继续编辑草稿时，停止输入约两秒后会自动保存收件、抄送和密送地址、主题、正文和附件。关闭写信窗口时会先保存未完成的更改。看到「草稿已保存」后，可以从草稿箱或另一台设备继续编辑。

保存遇到网络故障或其他窗口的版本冲突时，写信窗口会保留内容并显示原因。可以用「保存草稿」重试，或选择「放弃未保存更改」关闭。定时发送时间会在确认定时发送时提交。

## 定时发送

网页写信时点击「定时发送」，按设备时区选择将来的发送时间，再点击「确认定时发送」。收件人可以是站内账号，外部地址需要配置 SMTP 中继。邮件会保存在发件箱，Harbor Mail 服务到时处理。关闭浏览器后，计划仍有效。

在发件箱打开等待定时发送的邮件，可查看计划时间并选择「取消并转为草稿」。草稿保留编辑内容和附件，可修改后重新发送或安排新时间。服务重启后会继续处理计划，投递期限从计划时间起算五天。

### 撤销发送

在「邮箱偏好」里将撤销发送等待时间设为 5、10、20 或 30 秒。启用后，网页点击「发送邮件」会先把邮件暂存到发件箱。等待期间可点击页面下方的「撤销发送」，也可从发件箱撤销。到期后，撤销入口关闭，尚未开始投递的邮件显示「等待投递」。撤销成功后，原收件人、主题、正文和附件会回到草稿，可继续编辑。

等待从邮件进入发件队列时开始。期限结束时发件箱会显示「等待投递」，并关闭撤销入口。关闭网页或重启服务后，邮件仍会到期投递。设置默认关闭，明确选择的定时发送继续使用设置的发送时间。

## 追踪外部邮件投递

Web 邮箱或 SMTP 客户端提交的外部邮件会先保存到数据库。网页显示「已加入发件箱」表示保管成功，待中继明确接收后才会出现在「已发送」。

- 网络故障或中继返回临时错误时，发件箱显示下次尝试时间。重试间隔从 30 分钟逐步延长至最多 6 小时，最长保留 5 天的重试窗口。
- 中继在 SMTP AUTH 阶段拒绝登录时，邮件保留在队列中。管理员修正中继凭据后，可在投递队列提前重试。
- 中继永久拒绝或超过重试期限时，发件箱保留原邮件和失败状态，并在站内收件箱生成通知。移除中继后，待投递邮件仍按原期限结案。如果所有收件人的投递结果都已记录，服务重启后会根据结果直接结案，无需连接中继。
- 已提交 DATA 结束标记却没收到最终确认时，该地址标记为「结果待确认」。服务中断后会在投递租约到期时恢复。若还有其他地址等待投递，保留它们原有的重试时间。没有待投递地址时，整封邮件显示「结果待确认」。先查中继记录再决定是否重发，系统不会自动重投待确认的地址。发件箱支持下载原始 `.eml`。

队列数据随 SQLite 或 MySQL 一起备份。站内邮件仍即时投递。

## 阅读 HTML 邮件

网页支持在纯文本与 HTML 正文之间切换。只有 HTML 正文的邮件会自动打开 HTML 视图。HTML 在独立沙箱中显示，保留常见的表格、文字样式和通过 Content-ID 引用的 PNG、JPEG、GIF。远程图片不会自动加载；链接在点击后单独打开。

显示上限为 512 KiB 的 HTML 正文和总计约 2 MiB 的内嵌图片，单张图片最多 1 MiB。超限内容和完整原始格式可通过下载 `.eml` 获取。旧邮件可直接打开 HTML，无需迁移数据库。

## 邮件导入与导出

网页邮件列表的「导入导出」可将一个或多个 EML 文件导入收件箱、已发送、归档或自定义文件夹，也可把一个文件夹导出为包含原始 EML 的 ZIP。可在「批量整理」中先选邮件，再执行「导出所选 ZIP」，从大文件夹分批导出。

每批最多 100 封、合计 64 MiB，导入时单封最多 10 MiB。导入保留原始 MIME 和附件，并使用 EML 中可解析的日期。导入按目标文件夹建立新副本，导入到已发送的邮件标为已读，其余标为未读。ZIP 解压后可选择其中的 EML 导入。通过反向代理上传时，请将请求体限额配到至少 65 MiB。

## 当前范围

网页提供附件与原始邮件下载。网页发信支持纯文本正文及最多 10 个附件，附件合计不超过 6 MiB，编码后的整封邮件和 SMTP 来信上限均为 10 MiB。通过反向代理部署时，请确认其请求体限额至少为 7 MiB。

生产部署的磁盘上限、监控告警、SQLite 和 MySQL 备份及恢复步骤见 [生产运行手册](../ops/README.md)。
