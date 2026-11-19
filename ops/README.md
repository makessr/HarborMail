# Harbor Mail 生产运行

以下模板面向 Linux Docker 主机。备份和恢复命令都从项目目录运行，按实际启用的覆盖文件补齐 `-f` 参数。为生产和演练各用独立的 Compose 项目、数据库、数据目录及监听地址。

## 首次启动与上线验收

新实例先用[基础 Compose 配置](../docs/deployment-and-usage.md#启动-mysql-与-redis)完成启动和本机验收，再接入公网。Linux 上可用下面的 Bash 示例准备凭据，需要 OpenSSL。从项目目录执行，先把示例主邮箱换成自己的地址；已有实例沿用现有凭据。

```bash
(
  set -e
  umask 077
  mkdir -p secrets
  docker compose -f compose.yaml build harbor
  read -rsp '输入邮箱密码（12–72 字节）: ' mailbox_password
  printf '\n'
  account_hash=$(printf '%s\n' "$mailbox_password" | docker run --rm -i harbormail:local hash-password)
  unset mailbox_password
  printf '{"hello@harbor.test":"%s"}\n' "$account_hash" > secrets/accounts.json
  mysql_password=$(openssl rand -hex 24)
  mysql_root_password=$(openssl rand -hex 24)
  printf '%s' "$mysql_password" > secrets/mysql-password
  printf '%s' "$mysql_root_password" > secrets/mysql-root-password
  printf 'harbor:%s@tcp(mysql:3306)/harbor_mail' "$mysql_password" > secrets/mysql-dsn
)
```

启动前确认 Harbor 容器的 UID/GID `10001:10001` 能读取 `accounts.json` 和 `mysql-dsn`。可在标准 Linux Docker 主机上把这两个文件的组设为 `10001`、权限设为 `0640`。保留主机凭据目录的访问限制，并在 `.env` 写入实际的 `HARBOR_DOMAIN` 和 `HARBOR_ADMINS`。

准备好下文的挂载和备份账号配置后，使用相应的生产 Compose 文件组合启动。上线前从外部网络核对 MX 与 TCP 25 连通性、HTTPS 与邮件客户端证书、实际收发和备份恢复。Web 反向代理应允许 `/api/events` 的 SSE 长连接通过。 [域名、DNS 与 TLS 配置](../docs/deployment-and-usage.md#接入真实域名)

### 多实例网页同步验收

使用两个 Harbor Mail 实例连接同一 MySQL 库，两个实例使用相同的账号与管理员认证配置。单机演练时分别映射不同的 Web 和 SMTP 回环端口，避免固定宿主机端口冲突。在实例 B 打开网页收件箱，向实例 A 的 SMTP 端口投递一封测试邮件。实例 B 应在约 5 秒内通过 `/api/events` 收到刷新事件，并显示新邮件。用另一个账号核对它的收件箱。部署反向代理后，从实际 HTTPS 入口再验收一遍，确认代理关闭 SSE 响应缓冲并放行 15 秒心跳。

## 存储上限

为数据挂载独立且容量固定的文件系统，例如 LVM 逻辑卷或云盘。将挂载写入主机的持久配置，并为 Compose 启动服务设置相应的 systemd `RequiresMountsFor=`。启动前用 `findmnt --mountpoint` 检查挂载来源，避免主机重启后数据被写入根分区。备份放在另一块文件系统，并保留异地副本。

| 挂载示例 | 内容 | 可调整的起点 |
| --- | --- | --- |
| `/srv/harbor-data` | SQLite 数据库及 WAL，或 MySQL 模式下的 Harbor 数据，含 ACME 缓存 | SQLite 50 GiB，MySQL 模式 2 GiB |
| `/srv/harbor-mysql` | MySQL 数据、日志和临时增长 | 100 GiB |
| `/srv/harbor-backup` | 快照、转储和校验文件 | 按保留期预留，至少容纳一次完整生成和一次恢复校验 |

这些是实例存储上限。容量应覆盖原始 MIME、目录或客户端复制、投递队列、索引、WAL 或 MySQL binlog，以及增长余量。不要依靠删除邮件立即回收数据库文件。使用空间达到 80% 时安排扩容，达到 90% 时当班处理并检查写失败。禁止手动删除 WAL、binlog 或数据库内部文件。

在 `.env` 填写 `HARBOR_DATA_PATH=/srv/harbor-data`，MySQL 同时填写 `HARBOR_MYSQL_DATA_PATH=/srv/harbor-mysql`。Harbor 数据目录须可由 UID/GID `10001:10001` 写入。MySQL 目录须符合部署所用镜像的 `mysql` 用户权限。预先创建目录和挂载。覆盖文件按以下顺序加载。

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
| 已到期邮件仍未领取 | 最老 5 分钟且持续 5 分钟。检查出站 worker 日志、数据库和 SMTP 中继 |
| 投递结果未知记录增长 | 核对中继日志及实际投递结果，再决定是否重发 |
| 垃圾训练失败记录增长 | 检查 Rspamd 控制器的连接、认证和配置 |
| 挂载可用空间低于 20% | 10 分钟。用 `df -h`、`df -i` 和数据库日志定位增长 |
| 备份完成时间缺失或超过 26 小时 | 检查调度、空间、上传和最后可恢复的副本 |

另从接入网络探测 SMTP 25、提交端口、IMAPS、POP3S 和公开 Web HTTPS。启用证书到期日告警，例如剩余 21 天。检查这些探针自身和告警接收方的心跳。项目例程使用 Docker `local` 日志轮转，MySQL binlog、外部代理日志和监控数据仍需各自限制保留期。

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

按实际安装路径调整 `ops/harbor-backup.service` 和 `ops/harbor-backup.timer`。这两个模板只调度备份。在单独的维护任务中先运行 `bash ops/backup.sh plan` 查看保留计划，再使用具备仓库删除权限的凭据运行 `bash ops/backup.sh prune` 执行 14 个日备和 4 个周备保留策略。

用 `bash ops/backup.sh verify SNAPSHOT_ID` 验证指定快照。SQLite 会校验数据库完整性。MySQL 需要在隔离服务器预建空的 `harbor_verify_*` 数据库，设置 `HARBOR_BACKUP_VERIFY_DB` 和 `HARBOR_BACKUP_VERIFY_CNF`，验证账户只应有该库权限。脚本导入后读取邮件和队列计数，保留验证库供复查。自动转储不带 MySQL 触发器，Harbor 首次从恢复库启动时会按已启用的配额设置重建触发器和用量，避免恢复时依赖原服务器的触发器属主。自建触发器、部署配置及配套密钥按下文独立备份。[Restic 相对路径](https://restic.readthedocs.io/en/stable/040_backup.html#absolute-and-relative-paths) · [Restic 保留策略](https://restic.readthedocs.io/en/stable/060_forget.html)

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

以 MySQL 8.4 客户端转储。运行时暂停结构迁移。使用 `--hex-blob` 保留邮件原始数据。将下列文件参数和命令同样配置给每日调度任务。[MySQL 转储选项](https://dev.mysql.com/doc/refman/8.4/en/mysqldump.html)

```sh
umask 077
docker compose -f compose.yaml -f compose.mysql.yaml -f compose.ops.yaml -f compose.mysql.ops.yaml exec -T mysql mysqldump --defaults-extra-file=/run/secrets/mysql_backup_client --single-transaction --quick --no-tablespaces --set-gtid-purged=OFF --hex-blob --max-allowed-packet=64M harbor_mail > /srv/harbor-backup/harbor-YYYYMMDDTHHMMSSZ.sql.part
```

检查退出状态和转储体积，成功后改名为 `.sql`，计算 SHA-256，再上传加密异地存储。通过恢复演练验证转储可导入。外部托管 MySQL 可用供应商快照，或从受控备份主机用同等参数连接数据库，保留完整 TLS 验证。其存储和 binlog 告警由供应商或数据库监控提供。

### 成功标记与配套数据

数据库快照和异地副本确认可读后，使用 `sha256sum` 校验回读样本。把成功时间写到 node_exporter 目录下临时文件，原子改名为 `harbor-backup.prom`。该目录应允许备份账户写、node_exporter 读，和其他用途隔离。[node_exporter textfile collector](https://github.com/prometheus/node_exporter#textfile-collector)

```sh
printf 'harbor_backup_last_success_timestamp_seconds %s\n' "$(date +%s)" > /var/lib/node_exporter/textfile/harbor-backup.tmp
chmod 0644 /var/lib/node_exporter/textfile/harbor-backup.tmp
mv /var/lib/node_exporter/textfile/harbor-backup.tmp /var/lib/node_exporter/textfile/harbor-backup.prom
```

另外加密备份部署环境、账号与别名配置、应用及中继凭据、TLS PEM 或 ACME 缓存、代理配置。ACME 缓存内含私钥，须限制访问。Rspamd 训练数据按其后端的备份流程处理，数据库中的反馈状态无法替代已训练的数据。不要在异地副本报告或监控标签里写凭据。

## 恢复与校验

1. 先在隔离环境校验 SHA-256。保留当前数据库和卷，阻断演练环境的公网 SMTP 与中继出站流量。记录备份时间以及恢复期间的收信、退信和待投递队列。
2. SQLite 在停服后将已校验快照放入新的数据目录并命名为 `harbor.db`。给目录和文件设置正确属主及文件权限。恢复目标目录只含该快照的配套文件，不能混入原数据库的 `-wal` 和 `-shm`。用 `sqlite3 -readonly ... 'PRAGMA integrity_check;'` 验证副本。
3. MySQL 创建空库，恢复账户按需具有建表和导入权限。执行 `mysql --defaults-extra-file=/受限目录/restore.cnf --max-allowed-packet=64M harbor_mail < /备份目录/harbor-日期.sql`。核对导入退出状态和服务端日志，确认必要的应用账号及最小权限已恢复。
4. 用只读 SQL 核对 `letters`、`outbox`、`outbox_schedule`、`custom_folders`、`web_letter_recipients` 和 `web_outbox_envelopes` 的记录数和样本。核对后台账号 `managed_accounts`、账号状态 `account_access`、密码重置状态 `account_password_overrides`、后台别名 `managed_aliases` 和管理审计表。开放服务前核对备份之后的账号创建、停用、密码重置、凭据撤销和别名变更。启动投递进程前，先用中继日志核对旧队列结果，并确定已实际投递任务的处置方式，防止重复投递。
5. 在隔离恢复副本上以 `HARBOR_RECOVERY_MODE=true` 启动 Harbor。Compose 部署把该值设置在演练环境的 `.env` 中，使用该演练项目对应的 Compose 文件启动。核对日志中的 `Recovery mode`、健康端点、网页登录、旧邮件及附件、已发与定时邮件。校验模式保留登录与退出，屏蔽 Web 数据修改，停用 SMTP、提交、IMAP、POP3、投递、稍后处理与垃圾邮件训练，要求关闭自动 ACME。应用启动仍可能迁移数据库，网页登录仍会写入会话。只对恢复副本使用该模式。
6. 用只读 SQL 核对恢复的 IMAP 目录和 UID。需要用客户端验收时，另用隔离副本并处置旧队列，确认网络出口已阻断后，关闭校验模式启动协议服务。记录恢复耗时和最新可读数据时间。生产切换要停止所有旧实例再切换数据路径或 DSN，先确认健康和队列处置，再逐步开放流量。
