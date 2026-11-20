# 反垃圾邮件与杀毒

Harbor Mail 对接 Rspamd 扫描公网 SMTP 来信，并可提交用户分类反馈。需要杀毒时，可让 Rspamd 的 antivirus 模块调用 ClamAV。两项均为可选组件。

## Docker 一键启用

按[快速部署](deployment-and-usage.md#docker-compose-部署)完成初始配置后，在 `.env` 的 `COMPOSE_FILE` 末尾追加 `:compose.mail-scan.yaml`。Windows 开发机沿用 Compose 的分隔符 `;`。

```bash
docker compose up -d --no-build --wait --wait-timeout 900
docker compose ps
```

首次启动需要下载镜像、更新病毒库并加载签名，可能需要数分钟。Rspamd、ClamAV 和扫描专用 Redis 均不发布宿主机端口。病毒库、Rspamd 和扫描 Redis 分别使用持久卷，扫描使用的 Redis 与 Harbor Mail 后台缓存分开。内置控制器仅监听容器回环地址，如需用户分类训练，可按原有训练配置接入可信的专用控制器。 [Rspamd 容器](https://docs.rspamd.com/getting-started/installation/#docker-installation) · [ClamAV 容器](https://docs.clamav.net/manual/Installing/Docker.html)

默认启用病毒隔离和扫描故障临时拒收。进入后台「隔离邮件」可查看信封发件人、收件人、邮件时间和大小。核实误报后可释放到原收件人的「垃圾邮件」，也可永久删除。两项操作都需要全站管理员重新验证密码，并记入操作审计。隔离区最多 1,000 封、合计 256 MiB，达到上限后需从后台清理，新增隔离邮件会收到 SMTP 451。

## 对接自管 Rspamd

1. 按上游安装说明部署 Rspamd，启用 Harbor Mail 能访问的私有 normal worker。Docker 可将两个服务加入同一可信网络。Linux 原生部署可使用本机回环监听。[Rspamd 安装指南](https://docs.rspamd.com/getting-started/installation/)
2. Docker 在 `.env` 设置 `HARBOR_RSPAMD_URL=http://rspamd:11333/checkv2`，用实际可解析的服务名替换 `rspamd`，再运行 `docker compose up -d --no-build --wait`。systemd 部署在 `/etc/harbormail/harbormail.env` 配置，例如 `HARBOR_RSPAMD_URL=http://127.0.0.1:11333/checkv2`，然后重启 Harbor Mail。
3. 从 Harbor Mail 可访问的网络向扫描 worker 提交专用测试邮件，查看 JSON 的 `action`。再通过 SMTP 向演练账号投递，确认邮件出现在预期文件夹。训练另行配置控制器、授权账号和凭据文件。[扫描协议](https://docs.rspamd.com/developers/protocol/#normal-worker-http-endpoints) · [训练配置](deployment-and-usage.md#分类训练)

```bash
curl -fsS --max-time 5 --data-binary @sample.eml http://127.0.0.1:11333/checkv2
```

示例中的回环地址适用于 Rspamd 与测试命令运行在同一主机时。跨容器使用私有服务地址。扫描 worker、训练控制器及 ClamAV 的 clamd 端口应留在可信网络内。Harbor Mail 的后台缓存 Redis 使用专用权限，Rspamd 的训练与缓存使用其独立后端。

### Docker 跨 Compose 项目连接

如果已有 Rspamd Compose 项目，可先运行 `docker network create harbor-mail-scan`。在 Rspamd 项目的配置中将扫描服务加入这个外部网络，并保留它原有的网络。Harbor Mail 也加载一个覆盖文件，例如 `compose.scan-network.yaml`，加到 `.env` 中的 `COMPOSE_FILE` 末尾。

```yaml
services:
  harbor:
    networks:
      - mail_scan
networks:
  mail_scan:
    external: true
    name: harbor-mail-scan
```

执行 `docker compose config`，确认 Harbor Mail 同时保留数据库和缓存需要的网络。然后执行 `docker compose up -d --no-build --wait`。扫描服务在共享网络的别名应和 `HARBOR_RSPAMD_URL` 一致。

## 通过 Rspamd 对接 ClamAV

1. 部署 ClamAV 的 `clamd`，确保病毒库可持续更新。容器可为 `/var/lib/clamav` 配置持久卷，并让 Rspamd 通过私有容器网络连接 `clamav:3310`。[ClamAV 容器与病毒库](https://docs.clamav.net/manual/Installing/Docker.html)
2. 在 Rspamd 的 `/etc/rspamd/local.d/antivirus.conf` 添加下列配置。通过 Unix socket 连接时，把 `servers` 换成 Rspamd 可访问的 socket 路径。[Rspamd antivirus 配置](https://docs.rspamd.com/modules/antivirus/)

```ucl
clamav {
  type = "clamav";
  servers = "clamav:3310";
  symbol = "CLAM_VIRUS";
  action = "reject";
  timeout = 3;
}
```

3. 运行 `rspamadm configtest` 后重启 Rspamd。在隔离环境用 EICAR 标准测试附件核对 `CLAM_VIRUS`、Rspamd 的 action 和 Harbor Mail 的最终收件目录。[EICAR 测试文件](https://www.eicar.org/download-anti-malware-testfile/)

### 扫描范围与处置策略

| 配置项 | 自行对接默认值 | 一键部署默认值 | 可选行为 |
| --- | --- | --- | --- |
| `HARBOR_SPAM_ACTION` | `junk` | `junk` | `reject` 在公网 SMTP 的 DATA 阶段以 554 拒收 Rspamd 标为拒收、丢弃或隔离的垃圾邮件 |
| `HARBOR_VIRUS_ACTION` | `junk` | `quarantine` | `quarantine` 将 `CLAM_VIRUS` 命中交由全站管理员处置，`reject` 在 SMTP 阶段拒收，`junk` 进入原有垃圾邮件分类流程 |
| `HARBOR_SCAN_FAILURE` | `deliver` | `defer` | `defer` 遇到 HTTP 故障、扫描跳过、Rspamd `soft reject` 或 `CLAM_VIRUS_FAIL` 时返回 451，发送方可重试 |
| `HARBOR_RSPAMD_TIMEOUT_SECONDS` | `4` | `15` | HTTP 扫描超时，范围 1 至 30 秒 |

普通垃圾邮件分类仍可由用户规则覆盖。拒收和隔离在邮件进入邮箱前完成。使用自管 ClamAV 时，请保持 `CLAM_VIRUS` 和 `CLAM_VIRUS_FAIL` 符号，分别用于标记命中和故障。确认扫描服务及其上游大小限制能覆盖 Harbor Mail 接收上限。

扫描覆盖公网 SMTP 入口。网页发信、SMTP Submission、站内投递、EML 导入和 IMAP 迁移沿用各自的现有路径。
