# 反垃圾邮件与杀毒

Harbor Mail 对接 Rspamd 扫描公网 SMTP 来信，并可提交用户分类反馈。需要杀毒时，可让 Rspamd 的 antivirus 模块调用 ClamAV。两项均为可选组件。

## 对接 Rspamd

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

### 扫描范围与故障行为

Harbor Mail 把 Rspamd 的 `reject`、`quarantine` 等动作作为垃圾邮件分类信号，邮件原文仍可保留，用户的收信移动规则也可能覆盖目标目录。Rspamd 超时或不可用时，Harbor Mail 继续按收件规则投递。Rspamd 日志中的 `CLAM_VIRUS_FAIL` 表示其杀毒检查出错。

需要在 SMTP 层拒收病毒邮件，或扫描器故障时延迟接受邮件，可部署独立 SMTP 网关在 Harbor Mail 前执行拒收或临时拒收策略。当前扫描只覆盖公网 SMTP 入口。网页发信、SMTP Submission、站内投递和 EML 导入需要在各自入口另行实施扫描策略。
