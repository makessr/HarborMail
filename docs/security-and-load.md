# 公网安全与负载复测

后台「安全 → 公网部署安全检查」仅对全站管理员开放。它检查会话 Cookie 的 Secure 属性、可信代理配置、当前管理员的部署托管两步验证、来信扫描和备份回读。使用反向代理时，仍需从外部 HTTPS 入口检查证书，并核对代理覆盖转发头。扫描探测使用合成邮件，不会投递到邮箱。 [公网部署](quick-start.md) · [扫描配置](spam-and-antivirus.md)

备份任务使用 `HARBOR_BACKUP_METRIC_DIR` 写入 `harbor-backup.prom` 后，可把 `HARBOR_BACKUP_STATUS_FILE` 设置为该文件的绝对路径。容器部署把指标**目录**只读挂载到容器，并设置环境变量指向容器内文件。备份脚本使用原子替换，挂载目录可以持续看到更新。该检查报告是否有 26 小时内的成功回读记录。 [备份与恢复](../ops/README.md#备份)

发布门禁使用 `govulncheck` 检查四个发行平台的 Go 调用路径。GHCR 候选镜像按摘要扫描 Linux amd64 和 arm64，保留 Trivy 高危和严重问题报告。有可修复的高危或严重问题时，流程停止更新 `latest` 和 GitHub 正式版。

## 数据库和搜索负载

以下命令在仓库根目录运行。SQLite 使用临时文件。加测 MySQL 时，先为演练准备独立的 `harbor_load_*` 数据库，并设置 `HARBOR_DATABASE_LOAD_MYSQL_DSN`。

```bash
HARBOR_DATABASE_LOAD=1 go test ./internal/server -run '^TestDatabaseMixedLoad$' -count=1 -v
```

每种模式使用 12,000 封合成历史邮件，运行 4 个并发收信写入任务和 8 个并发搜索任务，验证邮件落库与搜索结果。输出中文与中英混合搜索和收信入库的 P50、P95、最大耗时，以及连接池和 MySQL 行锁等待。该演练在实际数据库上运行邮箱收信和列表处理函数，适合比较查询和存储路径。
