# Harbor Mail 生产运行

以下模板面向 Linux 主机，支持 Docker Compose 和单二进制。备份和恢复命令都从项目目录运行。为生产和演练各用独立的数据库、数据目录及监听地址。

## Linux 单二进制 systemd 部署

使用 systemd 251 或更新版本。从项目目录准备目标架构的 `harbormail` 和 `secrets/accounts.json`，生成方式见[单二进制启动说明](../docs/deployment-and-usage.md#linux-单二进制)。也可在单文件的构建产物中下载并解压 `harbormail-systemd.tar.gz`，取得相同的 `ops/` 模板。首次安装时创建专用系统账号和配置目录。已有部署沿用原账号、凭据和数据路径。

```bash
getent passwd harbormail >/dev/null ||
  sudo useradd --system --user-group --home-dir /var/lib/harbormail --shell "$(command -v nologin)" harbormail
sudo install -d -m 0700 /etc/harbormail
sudo install -m 0755 ./harbormail /usr/local/bin/harbormail
sudo install -m 0600 secrets/accounts.json /etc/harbormail/accounts.json
sudo install -m 0600 ops/harbormail.env.example /etc/harbormail/harbormail.env
sudoedit /etc/harbormail/harbormail.env
sudo install -m 0644 ops/harbormail.service /etc/systemd/system/harbormail.service
sudo systemctl daemon-reload
sudo systemctl enable --now harbormail.service
```

模板默认使用本机回环端口 Web 8080 和 SMTP 2525，SQLite 数据库位于 `/var/lib/harbormail/harbor.db`。把示例域名改为实际值。systemd 创建可写的数据目录，并通过 `LoadCredential=` 让专用服务账号读取账号文件。启动后按实际监听地址检查 `/healthz`，默认可运行 `curl -fsS http://127.0.0.1:8080/healthz`。查看日志用 `sudo journalctl -u harbormail.service`。 [systemd 凭据与状态目录](https://www.freedesktop.org/software/systemd/man/252/systemd.exec.html)

### MySQL、Redis 与 TLS

使用 MySQL 时在 `harbormail.env` 里将 `HARBOR_DB_DRIVER` 设为 `mysql`。将 DSN 写入仅 root 可读的 `/etc/harbormail/mysql-dsn`，用 `sudo systemctl edit harbormail.service` 加入配置。

```ini
[Service]
LoadCredential=mysql-dsn:/etc/harbormail/mysql-dsn
Environment=HARBOR_MYSQL_DSN_FILE=%d/mysql-dsn
```

启用 Redis 时，在环境文件中设置专用 `HARBOR_REDIS_NAMESPACE`。将连接地址写入仅 root 可读的 `/etc/harbormail/redis-url`，在同一个 drop-in 的 `[Service]` 下添加 `LoadCredential=redis-url:/etc/harbormail/redis-url` 和 `Environment=HARBOR_REDIS_URL_FILE=%d/redis-url`。 [Redis 配置](../docs/deployment-and-usage.md#可选-redis-缓存)

需要直接绑定 TCP 25、465 等低端口时，在 drop-in 的 `[Service]` 添加 `CapabilityBoundingSet=CAP_NET_BIND_SERVICE` 和 `AmbientCapabilities=CAP_NET_BIND_SERVICE`，再配置对应的监听地址。使用 `LoadCredential=` 时，将 `HARBOR_CERT`、`HARBOR_KEY` 指向相应的 `%d/` 路径，更新后重启服务以刷新 systemd 凭据快照。需要热更新时，把两项指向服务账号可读的常规文件，例如 `/etc/harbormail/tls/` 下由受限组权限保护的证书和私钥。用 ACME 时将 `HARBOR_ACME_CACHE` 指向状态目录下的可写路径，如 `/var/lib/harbormail/acme`。公网接入时核对实际端口、DNS、反向代理及 TLS。 [轮换 PEM 证书](../docs/deployment-and-usage.md#轮换-pem-证书)

更新前备份并保留旧二进制，执行 `sudo systemctl stop harbormail.service`，安装新版文件后再启动并检查健康状态。修改服务单元后先执行 `sudo systemctl daemon-reload`。模板为有序停机保留 125 秒。

## 首次启动与上线验收

首次部署按[三步 Docker 部署](../docs/deployment-and-usage.md#docker-compose-部署)生成凭据、启动并验证登录。已有实例沿用原有凭据和数据卷。

配置好生产挂载和备份账号后，从外部网络核对 MX 与 TCP 25 连通性、HTTPS 与邮件客户端证书、实际收发和备份恢复。Web 反向代理应允许 `/api/events` 的 SSE 长连接通过。 [域名、DNS 与 TLS 配置](../docs/deployment-and-usage.md#接入真实域名)

### MySQL 预检

MySQL 服务启动后，在首次部署、升级或更换数据库地址时，用实际部署的 Compose 文件和应用凭据执行预检。

```bash
docker compose -f compose.yaml -f compose.mysql.yaml -f compose.registry.yaml \
  run --rm --no-deps -T harbor mysql-check
```

单二进制部署使用与服务相同的 `HARBOR_MYSQL_DSN_FILE` 或 `HARBOR_MYSQL_DSN`，执行 `/usr/local/bin/harbormail mysql-check`。检查最多 20 秒。命令检查连接、实例和会话的只读设置、数据包下限，以及现有 `letters` 表的基本读写权限。它使用 `EXPLAIN` 检查权限，全程只查询数据库。检查失败返回非零退出码。启用硬配额时也会提示二进制日志的触发器策略。

空库通过预检时会明确提示尚未验证表权限，初始化后请再运行一次。建表、建索引和建触发器权限及生产数据容量，仍需用相同账号在隔离环境演练。 [MySQL EXPLAIN](https://dev.mysql.com/doc/refman/8.4/en/explain.html) · [MySQL 数据包限制](https://dev.mysql.com/doc/refman/8.4/en/server-system-variables.html#sysvar_max_allowed_packet)

### 邮件域名 DNS 预检

配置好邮件域名的 DNS 后，从部署主机运行 `harbormail dns-check example.com selector1`。`selector1` 换成中继提供的 DKIM 选择器。暂时没有选择器时，可省略最后一个参数。Docker Compose 部署可运行：

```bash
docker compose run --rm --no-deps -T harbor dns-check example.com selector1
```

命令查询当前环境解析器看到的记录，约五秒内输出 JSON。MX、SPF、该域的 DMARC 及手动指定的 DKIM 均找到时返回 0。`missing` 表示缺失，`multiple` 表示有多条匹配的 TXT，`null` 表示域名明确拒收，`error` 表示查询失败，`skipped` 表示未提供 DKIM 选择器。预检只核对记录是否存在及是否重复，实信的签名、SPF 发信授权、DMARC 对齐和投递仍需验收。 [Null MX 标准](https://www.rfc-editor.org/rfc/rfc7505.html)

### 启用内置 DKIM

内置签名默认关闭。首次启用时，在部署目录生成一次 32 字节加密密钥。已配置 `HARBOR_SECRETS_DIR` 时使用同一目录。

```bash
secrets_dir=${HARBOR_SECRETS_DIR:-./secrets}
(umask 077; set -C; openssl rand -hex 32 > "$secrets_dir/dkim-key")
```

确保容器内 UID `10001` 可读该文件。在 `.env` 的 `COMPOSE_FILE` 末尾加 `:compose.dkim.yaml`，然后运行 `docker compose up -d --no-build --wait`。单二进制部署把 `HARBOR_DKIM_KEY_FILE` 指向服务账号可读的密钥文件。systemd 可以用 `LoadCredential=dkim-key:/etc/harbormail/dkim-key` 和 `Environment=HARBOR_DKIM_KEY_FILE=%d/dkim-key` 挂载 root 专用文件。共享 MySQL 的所有实例使用同一密钥，并在开启签名前统一升级。

在「管理后台 → 域名 → DKIM」生成密钥。把显示的记录名和值添加到 DNS，然后选择「检查 DNS 并启用」。Harbor Mail 会验证公钥一致，再启用该域的 RSA-SHA256 2048 位签名。轮换时先生成并发布新记录，再启用新选择器。为已经发出的邮件保留旧公钥记录。

运行 `docker compose run --rm --no-deps -T harbor dns-check example.com --configured` 可用指定域名的当前选择器做预检。内置签名仍通过既有 SMTP 中继外发。向外部邮箱发一封测试邮件，在收件方查看 `Authentication-Results`，确认 `dkim=pass` 且签名域是你的域名。若中继会添加页脚或改写正文，需要让它保留原文，或由中继按改写后的内容完成 DKIM 签名。

备份数据库时单独离线备份 `dkim-key`。数据库存储的是加密私钥，恢复时使用原来的加密密钥。启用签名的域在密钥缺失或无法解密时会等待重试。邮件的 From 地址须与 SMTP 信封发件地址一致。 [DKIM 密钥要求](https://www.rfc-editor.org/rfc/rfc8301.html)

#### DKIM 上线预检

升级实例、恢复备份或更换密钥文件后，从每个部署实例用实际的数据库配置和 DKIM 密钥检查当前签名配置。

```bash
docker compose run --rm --no-deps -T harbor dkim-check example.com
```

单二进制部署执行 `harbormail dkim-check example.com`。轮换时追加 `--pending` 检查待启用的选择器。命令只读取配置和 DNS，不改动密钥或发送邮件。JSON 输出中的 `keyState` 和 `dnsState` 均为 `match` 才返回 0。`secret_missing` 或 `secret_unusable` 表示应核对密钥文件挂载和格式，`decrypt_failed` 表示文件中的密钥无法解密库里的私钥。DNS 侧会区分缺失、重复、内容不匹配或查询失败。正式投递仍以收件方返回的验签结果为准。 [DKIM 公钥查询](https://www.rfc-editor.org/rfc/rfc6376.html#section-3.6.2.1)

#### 收件原文验签

向外部邮箱发送一封测试邮件，从收件方下载原始 `.eml`，保留完整邮件头和正文。用要确认的签名域和选择器执行验签。省略选择器时，确认该域任一签名通过即可。

```bash
docker compose run --rm --no-deps -T harbor dkim-verify example.com SELECTOR < received.eml
```

单二进制部署使用 `harbormail dkim-verify example.com SELECTOR < received.eml`。只有 `status: pass` 返回 0。`no_signature` 表示没有该域的签名，`fail` 表示该域的签名未通过，也可能是使用了其他选择器。DNS 暂时故障以 `temporary_error` 报告。命令最多读取 32 MiB 的 `.eml`，并只为指定的签名身份查询 DNS 公钥。结果仅说明 DKIM 是否通过，SPF、DMARC 和收件方的投递决策可查看收件方记录。 [DKIM 验证规范](https://www.rfc-editor.org/rfc/rfc6376.html#section-6)

### SMTP 中继预检

为每个使用外发中继的发件域选一个发件地址。使用部署时相同的 Compose 文件、项目名及环境配置，保留已启用的中继认证、按域中继和私有 CA 覆盖文件。下面是已配置全局中继认证时的示例。 [Docker Compose run](https://docs.docker.com/reference/cli/docker/compose/run/)

```bash
docker compose -f compose.yaml -f compose.mysql.yaml -f compose.registry.yaml \
  -f compose.relay-auth.yaml run --rm --no-deps -T harbor relay-check hello@example.com
```

单二进制部署使用服务的运行环境和可读的凭据文件执行 `/usr/local/bin/harbormail relay-check hello@example.com`。按域配置时，命令选取对应的专用中继。其余地址沿用全局中继。

命令显示选中的中继、TLS 模式和认证结果。每次网络检查最多 20 秒。需要确认中继是否在信封阶段接受发件地址时，在地址后加 `--mail-from`，例如 `harbormail relay-check hello@example.com --mail-from`。命令在已验证的 TLS 连接上执行 `MAIL FROM`，接受后立即通过 `RSET` 取消事务，不提交收件人或正文。检查通过返回 0，失败返回非零状态和对应阶段。 [SMTP RSET](https://www.rfc-editor.org/rfc/rfc5321.html#section-4.1.1.5)

检查不读取数据库或队列。中继可能在收件人或正文阶段继续校验，实际公网收发以及 SPF、DKIM、DMARC 结果仍需实信验收。

CI 用两个独立 Harbor Mail 实例，验证默认中继、按域中继和跨实例回复。演练覆盖 SMTP AUTH、STARTTLS、隐式 TLS，以及发件归档、收件入箱和回复的邮件线程标头。

### 多实例网页同步验收

使用两个 Harbor Mail 实例连接同一 MySQL 库，两个实例使用相同的账号与管理员认证配置。单机演练时分别映射不同的 Web 和 SMTP 回环端口，避免固定宿主机端口冲突。在实例 B 打开网页收件箱，向实例 A 的 SMTP 端口投递一封测试邮件。实例 B 应在约 5 秒内通过 `/api/events` 收到刷新事件，并显示新邮件。用另一个账号核对它的收件箱。部署反向代理后，从实际 HTTPS 入口再验收一遍，确认代理关闭 SSE 响应缓冲并放行 15 秒心跳。

## 存储上限

为数据挂载独立且容量固定的文件系统，例如 LVM 逻辑卷或云盘。将挂载写入主机的持久配置，并为 Compose 启动服务设置相应的 systemd `RequiresMountsFor=`。启动前用 `findmnt --mountpoint` 检查挂载来源，避免主机重启后数据被写入根分区。备份放在另一块文件系统，并保留异地副本。

单文件与 Compose 的服务模板均保留 125 秒停机宽限。超过期限或进程异常退出时，发件箱继续用持久化状态和租约恢复。

| 挂载示例 | 内容 | 可调整的起点 |
| --- | --- | --- |
| `/srv/harbor-data` | SQLite 数据库及 WAL，或 MySQL 模式下的 Harbor 数据，含 ACME 缓存 | SQLite 50 GiB，MySQL 模式 2 GiB |
| `/srv/harbor-mysql` | MySQL 数据、日志和临时增长 | 100 GiB |
| `/srv/harbor-backup` | 快照、转储和校验文件 | 按保留期预留，至少容纳一次完整生成和一次恢复校验 |

这些是实例存储上限。容量应覆盖原始 MIME、目录或客户端复制、投递队列、索引、WAL 或 MySQL binlog，以及增长余量。不要依靠删除邮件立即回收数据库文件。使用空间达到 80% 时安排扩容，达到 90% 时当班处理并检查写失败。禁止手动删除 WAL、binlog 或数据库内部文件。

在 `.env` 填写 `HARBOR_DATA_PATH=/srv/harbor-data`，MySQL 同时填写 `HARBOR_MYSQL_DATA_PATH=/srv/harbor-mysql`。Harbor 数据目录须可由 UID/GID `10001:10001` 写入。MySQL 目录须符合部署所用镜像的 `mysql` 用户权限。

启动前创建目录并挂载文件系统，再用宿主机的 `findmnt` 核实挂载来源。Compose 会拒绝不存在的绑定源。使用 `compose.ops.yaml` 时，Harbor 在连接数据库、开放端口前以容器身份创建并删除一个临时文件。出现 `HARBOR_CHECK_DATA_DIR is not writable` 时，检查宿主机路径的属主、权限和挂载读写状态。

覆盖文件按以下顺序加载。

```text
docker compose -f compose.yaml -f compose.ops.yaml up -d --build --wait
docker compose -f compose.yaml -f compose.mysql.yaml -f compose.ops.yaml -f compose.mysql.ops.yaml up -d --build --wait
```

MySQL 示例依赖下文的备份客户端配置。已有实例迁移到绑定挂载前先完成备份，停止所有使用同一数据库的 Harbor 实例，并恢复到新挂载。原卷保留作回滚。完成恢复校验后再启用外部写入。Compose 上同一容器目标路径使用最后配置的挂载。[Docker Compose 合并规则](https://docs.docker.com/reference/compose-file/merge/)

MySQL Compose 部署同时启动 Redis，运行在 Harbor Mail 专用内部网络，读取缓存失败时由 MySQL 提供结果。缓存无持久化卷，恢复数据库时可重新建立缓存。若改用外部 Redis，请给生产与演练使用不同的缓存命名空间。

## 指标与告警

原生部署可设置 `HARBOR_METRICS=127.0.0.1:9091`。Compose 在主机回环发布端口，可用 `HARBOR_METRICS_PORT` 调整。指标不含账号标识或邮件字段。每次抓取有三秒数据库查询期限。保持此端口在可信监控网络内。

用同主机 Prometheus 加载 `prometheus.example.yaml` 与 `prometheus-rules.yaml`，另接入已有 Alertmanager 和通知渠道。让 node_exporter 在回环监听 9100，并设置 `--collector.textfile.directory=/var/lib/node_exporter/textfile`。如果修改挂载或指标端口，同时调整抓取配置或规则。确认 Prometheus 能看到预期文件系统序列。多实例共享 MySQL 时，队列指标为同库视图，仪表板按数据库取最大值，避免累加。

| 信号 | 起始阈值与处置 |
| --- | --- |
| 指标抓取连续失败 | 2 分钟。看 Harbor 容器、Web `/healthz`、数据库可达性和监控路径 |
| 邮件等待领取或恢复超时 | 到期或投递租约过期后等待超过 5 分钟，持续 5 分钟时告警。主动暂停投递或开启恢复模式的实例会继续提供队列指标，该实例暂不触发此告警。检查出站 worker 日志、数据库和 SMTP 中继 |
| 待确认的投递邮件数增长 | 按邮件计数。同一封邮件有多个待确认地址时只计一次，其他地址仍在重试时也计入。核对中继日志，再决定如何处置 |
| 垃圾训练失败记录增长 | 检查 Rspamd 控制器的连接、认证和配置 |
| 此实例存在不可用的 DKIM 私钥 | 持续 2 分钟时告警。到告警实例核对 Secret 挂载和数据库，运行 `dkim-check` 排查 |
| 挂载可用空间低于 20% | 10 分钟。用 `df -h`、`df -i` 和数据库日志定位增长 |
| 备份完成时间缺失或超过 26 小时 | 检查调度、空间、上传和最后可恢复的副本 |

项目例程使用 Docker `local` 日志轮转，MySQL binlog、外部代理日志和监控数据仍需各自限制保留期。

`harbor_dkim_active_domains` 统计已启用签名的域名数，`harbor_dkim_unusable_domains` 统计当前实例无法解密或无法与库中公钥配对的数量。共享 MySQL 时前者在各实例相同，后者应按实例查看。待启用的轮换密钥不计入。这两项指标不查询 DNS，也不输出域名、选择器或密钥内容。

在仓库根目录运行 `promtool test rules ops/prometheus-rules.test.yaml`，验证告警规则的触发和恢复。 [Prometheus 规则测试](https://prometheus.io/docs/prometheus/latest/configuration/unit_testing_rules/)

### 公网入口与证书监控

在能代表用户接入路径的监控主机运行 Blackbox Exporter。以下示例使用与 Prometheus 同主机的回环地址。探测接口接受目标地址参数，应仅对可信监控网络开放。 [Blackbox Exporter](https://github.com/prometheus/blackbox_exporter/tree/v0.29.1)

```bash
docker run -d --name harbor-blackbox --restart unless-stopped \
  --read-only --cap-drop ALL -p 127.0.0.1:9115:9115 \
  -v "$PWD/ops/blackbox.yaml:/etc/blackbox_exporter/config.yml:ro" \
  prom/blackbox-exporter:v0.29.1 --config.file=/etc/blackbox_exporter/config.yml
```

将 `prometheus-mail-probes.example.yaml` 复制到 Prometheus 配置目录并命名为 `prometheus-mail-probes.yaml`。替换示例域名与端口，移除部署中未启用的入口。取消主配置示例中 `scrape_config_files` 的注释。若探测器不在 Prometheus 主机上，把抓取示例的 `replacement` 改为可信网络地址。

| 入口 | 探测内容 |
| --- | --- |
| HTTPS | 从最终 HTTPS 地址请求 `/healthz`，验证成功响应、证书链及主机名 |
| SMTP 25 | 检查 SMTP 服务问候语 |
| SMTP 587 | 检查 STARTTLS、验证证书链及主机名，并在加密后再次执行 EHLO |
| SMTP 465、IMAPS 993、POP3S 995 | 完成隐式 TLS 握手并校验证书 |

探测失败或抓取失败持续三分钟触发严重告警。证书链中任一已呈现证书距到期不足 21 天且持续十分钟时触发警告。证书失效后，探测本身会失败。启用后在 Prometheus 核对 `probe_success`，并从 Alertmanager 确认测试通知可以送达。

## 备份

### 自动备份

Linux 主机可用 `bash ops/backup.sh backup` 生成数据库快照、上传到已初始化的加密 Restic 仓库，并完整回读核对 SHA-256。脚本依赖 Bash、util-linux 的 `flock`、GNU coreutils、`jq`、支持 `restore --verify` 的 Restic 和对应数据库客户端。将下列变量填入权限受限的 `/etc/harbor/backup.env`，让专用备份账户拥有暂存目录并设为 0700。预留空间容纳本地快照和回读副本。

| 环境变量 | 用途 |
| --- | --- |
| `HARBOR_BACKUP_INSTANCE` | 实例唯一名称，用于仓库标签与保留范围 |
| `HARBOR_BACKUP_WORK` | 备份账户独占的绝对暂存路径 |
| `RESTIC_REPOSITORY`、`RESTIC_PASSWORD_FILE` | 异地仓库地址及仓库口令文件 |
| `HARBOR_BACKUP_DRIVER` | `sqlite` 或 `mysql` |
| `HARBOR_BACKUP_SQLITE` | SQLite 源数据库绝对路径 |
| `HARBOR_BACKUP_MYSQL_DB`、`HARBOR_BACKUP_MYSQL_CNF` | MySQL 库名及受限客户端配置绝对路径 |
| `HARBOR_BACKUP_METRIC_DIR` | 可选，成功回读后原子更新 node_exporter 指标的目录 |

### 安装定时备份

`harbormail-systemd.tar.gz` 内含备份脚本、服务单元、定时器和环境模板。从项目目录或解包后的目录执行首次安装。服务会自动创建权限为 `0700` 的 `/var/lib/harbor-backup` 作为暂存目录。已有部署可在 `backup.env` 里保留原来的 `HARBOR_BACKUP_WORK`。

```bash
getent passwd harbor-backup >/dev/null ||
  sudo useradd --system --user-group --home-dir /var/lib/harbor-backup --shell "$(command -v nologin)" harbor-backup
sudo install -d -m 0755 /opt/harbor-mail/ops
sudo install -m 0755 ops/backup.sh /opt/harbor-mail/ops/backup.sh
sudo install -d -m 0700 /etc/harbor
sudo install -m 0600 ops/harbor-backup.env.example /etc/harbor/backup.env
sudoedit /etc/harbor/backup.env
sudo install -m 0644 ops/harbor-backup.service ops/harbor-backup.timer /etc/systemd/system/
```

在 `backup.env` 中填写实例名、已初始化的 Restic 仓库和数据库配置。将仓库口令保存为仅 root 可读的 `/etc/harbor/restic-password`。用 `sudo systemctl edit harbor-backup.service` 写入凭据路径。MySQL 模式可让 `harbor-backup` 用户使用仅 root 可读的客户端配置。

```ini
[Service]
LoadCredential=restic-password:/etc/harbor/restic-password
Environment=RESTIC_PASSWORD_FILE=%d/restic-password
LoadCredential=mysql-backup.cnf:/etc/harbor/mysql-backup.cnf
Environment=HARBOR_BACKUP_MYSQL_CNF=%d/mysql-backup.cnf
```

原生 SQLite 模式在 `backup.env` 设为 `HARBOR_BACKUP_DRIVER=sqlite`，并填写 `HARBOR_BACKUP_SQLITE`。在上述 drop-in 保留 Restic 两行，加入 `User=harbormail`。这样可以读取权限为 `0700` 的原生数据目录和正在使用的 SQLite WAL。Compose 的 SQLite 数据须按实际 UID 和挂载方式，确保备份账号能访问数据库及对应 WAL。开启备份成功指标时，指标目录还须供 node_exporter 读取。

#### 备份预检

首次启用定时备份或更换异地仓库时，可用实际备份账号运行 `bash ops/backup.sh check`。直接运行时沿用 `backup` 命令的环境和凭据。按上面的 MySQL systemd 示例安装后，可执行以下命令从 root 专用文件加载凭据。

```bash
sudo systemd-run --wait --pipe --collect -p User=harbor-backup \
  -p StateDirectory=harbor-backup -p StateDirectoryMode=0700 \
  -p EnvironmentFile=/etc/harbor/backup.env -p Environment=HARBOR_BACKUP_WORK=/var/lib/harbor-backup \
  -p LoadCredential=restic-password:/etc/harbor/restic-password \
  -p LoadCredential=mysql-backup.cnf:/etc/harbor/mysql-backup.cnf \
  /usr/bin/bash -c 'RESTIC_PASSWORD_FILE="$CREDENTIALS_DIRECTORY/restic-password" \
    HARBOR_BACKUP_MYSQL_CNF="$CREDENTIALS_DIRECTORY/mysql-backup.cnf" \
    exec bash /opt/harbor-mail/ops/backup.sh check'
```

原生 SQLite 使用 `-p User=harbormail`，并去掉 `mysql-backup.cnf` 和 `HARBOR_BACKUP_MYSQL_CNF` 对应的两行。自定义部署使用与备份服务相同的用户、路径和环境文件。命令检查源库必备表、暂存目录、可选指标目录及 Restic 仓库的读取和解密权限。预检只读取库结构和仓库配置，不生成快照或更新成功指标。仓库写入和回读仍须通过一次完整备份确认。 [Restic 命令手册](https://restic.readthedocs.io/en/stable/manual_rest.html)

#### 启动定时器

```bash
sudo systemctl daemon-reload
sudo systemctl start harbor-backup.service
sudo journalctl -u harbor-backup.service -n 30 --no-pager
sudo systemctl enable --now harbor-backup.timer
systemctl list-timers harbor-backup.timer
```

首次手动运行成功后，定时器按主机本地时间每天触发，随机延迟最多 30 分钟，补跑错过的日程。保留策略在独立维护任务执行。先用 `bash ops/backup.sh plan` 查看范围，再用具备仓库删除权限的凭据运行 `bash ops/backup.sh prune`，保留 14 个日备和 4 个周备。两种操作都按实例主机名和标签筛选。 [systemd 定时器](https://www.freedesktop.org/software/systemd/man/256/systemd.timer.html) · [Restic 保留策略](https://restic.readthedocs.io/en/stable/060_forget.html)

用 `bash ops/backup.sh verify SNAPSHOT_ID` 验证指定快照。恢复演练应沿用快照生成时的 `HARBOR_BACKUP_INSTANCE`，脚本会核对 Restic 主机名和实例标签。回读时只提取数据库产物和校验清单，并核对清单中的 SHA-256 对应这份数据库。

CI 使用带身份验证的独立 HTTPS Restic 仓库验证上传、恢复和保留策略。部署时先在实际异地仓库验证快照回读，再查看保留计划，确认范围后执行清理。可使用独立仓库口令，并通过受限配置为 REST 后端传入 `RESTIC_REST_USERNAME` 和 `RESTIC_REST_PASSWORD`。 [Restic REST 仓库配置](https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html#rest-server)

SQLite 和 MySQL 都会在上传、验证和导出时检查首个公开版本的必备表，覆盖账号、别名、文件夹与 IMAP UID、邮件和投递队列。检查失败时会列出缺失的表。SQLite 还会运行数据库完整性检查。早于首个公开版本的 SQLite 快照可能缺少这些表，应在隔离副本上升级后再生成新快照。

MySQL 备份账户需读取整个 Harbor 数据库，以涵盖后续版本增加的表。验证导入需在隔离服务器预建空的 `harbor_verify_*` 数据库，设置 `HARBOR_BACKUP_VERIFY_DB` 和 `HARBOR_BACKUP_VERIFY_CNF`，验证账户只应有该库权限。脚本导入后读取邮件和队列计数，保留验证库供复查。自动转储会跳过 MySQL 触发器，Harbor 首次从恢复库启动时会按已启用的配额设置重建触发器和用量，避免恢复时依赖原服务器的触发器属主。自建触发器、部署配置及配套密钥按下文独立备份。 [Restic 恢复选项](https://restic.readthedocs.io/en/stable/050_restore.html)

起始目标为每日一次，保留 14 个日备和 4 个周备，异地保存加密副本。此频率的恢复点目标为 24 小时，以演练量出的恢复耗时确定恢复时间目标。要求更小的丢失窗口时，增加备份频率或使用 MySQL 的 binlog 时间点恢复方案。

使用专门备份账户和受限目录。以下命令中的日期为示例，每次任务生成唯一 UTC 文件名。配置调度器串行运行，每步检查退出状态，任何一步失败都保留上次成功标记并告警。终止或中断产生的 `.part` 文件只用于排障。

### SQLite

安装与库格式兼容的 SQLite CLI。用 `.backup` 创建一致快照，避免仅复制活动数据库的主文件而遗漏 WAL。命令使用既有源文件，缺失时以错误退出。备份执行用户须有访问源数据库及 WAL 的权限。[SQLite 命令行文档](https://www.sqlite.org/cli.html)

```sh
umask 077
sqlite3 -bail -readonly /srv/harbor-data/harbor.db '.timeout 10000' '.backup /srv/harbor-backup/harbor-YYYYMMDDTHHMMSSZ.db.part'
sqlite3 -bail -readonly /srv/harbor-backup/harbor-YYYYMMDDTHHMMSSZ.db.part 'PRAGMA integrity_check;'
```

完整性检查只应返回 `ok`。确认后将文件改名为对应的 `.db`，生成 SHA-256 校验文件，再上传加密异地存储。

### MySQL

由 DBA 建立通过 Compose MySQL 容器本地 socket 登录的只读备份账户，授予 Harbor 库的 `SELECT`、`SHOW VIEW` 和 `TRIGGER`。将受限的 `secrets/mysql-backup.cnf` 配置为以下结构。只有备份执行者须能读取该配置。

```ini
[client]
user=harbor_backup
password=填入备份账号密码
protocol=socket
```

下面是使用 Compose 在 MySQL 容器内手动转储的示例。自动备份服务会在运行 `backup.sh` 的环境里直接调用 MySQL 客户端，因此其 `HARBOR_BACKUP_MYSQL_CNF` 必须指向该环境可读取的客户端配置，库地址或 socket 也须从该环境可达。转储期间暂停结构迁移，使用 `--hex-blob` 保留邮件原始数据。 [MySQL 转储选项](https://dev.mysql.com/doc/refman/8.4/en/mysqldump.html)

```sh
umask 077
docker compose -f compose.yaml -f compose.mysql.yaml -f compose.ops.yaml -f compose.mysql.ops.yaml exec -T mysql mysqldump --defaults-extra-file=/run/secrets/mysql_backup_client --single-transaction --quick --no-tablespaces --set-gtid-purged=OFF --hex-blob --max-allowed-packet=64M harbor_mail > /srv/harbor-backup/harbor-YYYYMMDDTHHMMSSZ.sql.part
```

检查退出状态和转储体积，成功后改名为 `.sql`，计算 SHA-256，再上传加密异地存储。通过恢复演练验证转储可导入。外部托管 MySQL 可用供应商快照，或从受控备份主机用同等参数连接数据库，保留完整 TLS 验证。其存储和 binlog 告警由供应商或数据库监控提供。

### 成功标记与配套数据

数据库快照和异地副本确认可读后，使用 `sha256sum` 校验回读样本。把成功时间写到 node_exporter 目录下临时文件，原子改名为 `harbor-backup.prom`。如果同名路径是目录，发布标记会报错，让调度器记录失败。指标目录应允许备份账户写、node_exporter 读，和其他用途隔离。 [node_exporter textfile collector](https://github.com/prometheus/node_exporter#textfile-collector)

```sh
printf 'harbor_backup_last_success_timestamp_seconds %s\n' "$(date +%s)" > /var/lib/node_exporter/textfile/harbor-backup.tmp
chmod 0644 /var/lib/node_exporter/textfile/harbor-backup.tmp
mv -T -- /var/lib/node_exporter/textfile/harbor-backup.tmp /var/lib/node_exporter/textfile/harbor-backup.prom
```

另外加密备份部署环境、账号与别名配置、应用及中继凭据、TLS PEM 或 ACME 缓存、代理配置。ACME 缓存内含私钥，须限制访问。Rspamd 训练数据按其后端的备份流程处理，数据库中的反馈状态无法替代已训练的数据。不要在异地副本报告或监控标签里写凭据。

## 恢复与校验

### 从 Restic 导出恢复包

在隔离的 Linux 环境沿用备份时的实例名和 Restic 仓库配置。将示例中的 `SNAPSHOT_ID` 换成记录里的快照 ID。目标必须是绝对路径，其父目录须已存在，且应由恢复操作账户控制。

```sh
bash ops/backup.sh export SNAPSHOT_ID /srv/harbor-recovery/restore-drill
(cd /srv/harbor-recovery/restore-drill && sha256sum -c SHA256SUMS)
```

导出成功后，目标目录包含 `harbor.db` 或 `harbor.sql`，以及 `SHA256SUMS`。脚本校验快照归属、校验和和必备表，成功后一次性创建目标目录。目录权限为 `0700`，文件权限为 `0600`。已有文件、目录和符号链接会使导出失败。MySQL 导出仅需仓库访问，实际导入按下面的隔离恢复步骤执行。

### 在隔离环境验证恢复

1. 先在隔离环境校验 SHA-256。保留当前数据库和卷，阻断演练环境的公网 SMTP 与中继出站流量。记录备份时间以及恢复期间的收信、退信和待投递队列。
2. SQLite 在停服后将已校验快照放入新的数据目录并命名为 `harbor.db`。给目录和文件设置正确属主及文件权限。恢复目标目录只含该快照的配套文件，不能混入原数据库的 `-wal` 和 `-shm`。用 `sqlite3 -readonly ... 'PRAGMA integrity_check;'` 验证副本。
3. MySQL 在隔离服务器创建空库，恢复账户按需具有建表和导入权限。执行 `mysql --defaults-extra-file=/受限目录/restore.cnf --max-allowed-packet=64M harbor_verify_drill < /srv/harbor-recovery/restore-drill/harbor.sql`。核对导入退出状态和服务端日志，确认必要的应用账号及最小权限已恢复。
4. 用只读 SQL 核对 `letters`、`outbox`、`outbox_schedule`、`custom_folders`、`web_letter_recipients` 和 `web_outbox_envelopes` 的记录数和样本。核对后台账号 `managed_accounts`、账号状态 `account_access`、密码重置状态 `account_password_overrides`、后台别名 `managed_aliases` 和管理审计表。开放服务前核对备份之后的账号创建、停用、密码重置、凭据撤销和别名变更。启动投递进程前，先用中继日志核对旧队列结果，并确定已实际投递任务的处置方式，防止重复投递。
5. 在隔离恢复副本上以 `HARBOR_RECOVERY_MODE=true` 启动 Harbor。Compose 部署把该值设置在演练环境的 `.env` 中，使用该演练项目对应的 Compose 文件启动。核对日志中的 `Recovery mode`、健康端点、网页登录、旧邮件及附件、已发与定时邮件。校验模式保留登录与退出，屏蔽 Web 数据修改，停用 SMTP、提交、IMAP、POP3、投递、稍后处理与垃圾邮件训练，要求关闭自动 ACME。应用启动仍可能迁移数据库，网页登录仍会写入会话。只对恢复副本使用该模式。
6. 用只读 SQL 核对恢复的 IMAP 目录和 UID。需要用客户端验收时，保持网络隔离，设置 `HARBOR_RECOVERY_MODE=false` 和 `HARBOR_PAUSE_OUTBOX=true` 后重建 Harbor 容器。此时网页与已配置的邮件协议可用，发件箱接受入队请求，定时发件和外发任务都继续等待。通过管理后台「运行监控」确认发件队列显示「暂停」。记录恢复耗时和最新可读数据时间。

### 切换与回滚

1. 暂停外部写入并停止所有连接原库的 Harbor 实例。记录原镜像版本、Compose 文件顺序、环境文件和数据路径。保留原数据目录或 MySQL 库及对应凭据。
2. 使用新的数据目录或恢复库启动。SQLite 绑定挂载可通过 `HARBOR_DATA_PATH` 指向新目录。MySQL 使用新的 DSN 文件，通过独立的 `HARBOR_SECRETS_DIR` 为对应部署指定文件。各实例统一设置 `HARBOR_PAUSE_OUTBOX=true`。保留原配置与数据，禁止让意外启动的旧实例访问恢复库。
3. 检查 `/healthz`、登录、样本邮件、邮件协议及发件箱。将恢复的队列与中继日志对照，处置已成功投递和结果待确认的任务。确认可切换后，在所有实例将 `HARBOR_PAUSE_OUTBOX` 改为 `false`，重建 Harbor 容器，再逐步开放流量。
4. 回滚时再次关闭外部写入并停止所有 Harbor 实例。若恢复库已接受新写入，先保留该库并记录新增邮件、队列及账号变更，以便对账。恢复原数据目录或 DSN，用支持暂停队列且与原库兼容的镜像启动。逐实例确认「发件队列」显示「暂停」，核对队列并决定如何处理分叉后，再恢复投递和外部写入。
