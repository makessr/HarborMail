# 发布检查清单

正式版本发布前，在候选提交上逐项执行，并在下方「执行记录」中写入该版本的实际结果和证据。检查项只有三种状态：**已执行**（附工作流链接、命令输出或截图）、**未执行**（写原因）、**本轮不适用**。没有执行过的项目不要勾选，也不要用复选框代替证据。

检查项里标注「自动」的由 CI 在候选提交上执行，人工只需确认运行结论；标注「人工」的必须在真实部署或本机演练中完成。

## 检查项

1. **主干回归（自动）**：`go test -race ./... -count=1`、独立 SMTP/IMAP/POP3 客户端测试、三个解析器模糊测试。见 `.github/workflows/verify.yaml` 的 `concurrency-and-parsing`。
2. **数据库演练（人工）**：合成数据下，SQLite 与 MySQL 各完成一次升级、回滚和备份恢复。共享 MySQL 的实例还要验证停写切换。
3. **站内路径（自动）**：Web、SMTP、IMAP、POP3 的站内收发由上面的自动化覆盖，并通过 `go test ./...` 在 SQLite 与隔离 MySQL 上各跑一遍。
4. **公网部署检查（人工）**：TLS 证书与续期、DNS 与 PTR、防火墙端口、可信代理、Secure Cookie、后台两步验证。管理后台「安全」页的检查项可作为清单。
5. **发行资产（自动 + 人工核对）**：Linux 与 Windows 的 amd64、arm64 二进制版本号正确；`SHA256SUMS` 覆盖 Release 全部资产且摘要一致；`OCI_IMAGE` 与 GHCR 上 `latest` 的索引摘要一致；`docs/releases/vX.Y.Z.md` 存在且内容对应该版本。
6. **镜像（自动）**：双架构构建、按架构冒烟（`version`、许可证文件、`hash-password`）、Trivy 对 HIGH/CRITICAL 校验「无可修复项」。见 `.github/actions/build-image/action.yaml`。
7. **按标签安装（人工）**：抽测 `vX.Y.Z` 标签下的 Compose 与 systemd 安装包，确认安装后版本号与标签一致。
8. **发布后核对（人工）**：检查下载资产和镜像摘要可访问，记录本版本的回滚路径和兼容性条件。
9. **文档与截图（人工）**：README 与部署文档里的界面截图对应当前版本，版本标注与本次发布一致；界面有改动时用 `npm run screenshots` 重拍并替换 `docs/assets/*.webp`。

## 执行记录

### v0.2.25（2026-11-23 发布）

| 检查项 | 状态 | 证据 |
| --- | --- | --- |
| 1. 主干回归 | 已执行 | Verify 工作流 [45180524527](https://github.com/makessr/HarborMail/actions/runs/45180524527)（main）与 [45180523185](https://github.com/makessr/HarborMail/actions/runs/45180523185)（v0.2.25）均成功，含 race、协议客户端、模糊测试；2026-11-27 本地 `go test ./... -count=1` 通过 |
| 2. 数据库演练 | 未执行 | 无 SQLite/MySQL 升级与回滚演练记录 |
| 3. 站内路径 | 已执行 | 同第 1 项，`verify` 作业在隔离 MySQL 与 SQLite 上各跑一遍；含 Chromium 网页回归与 Compose/Prometheus 配置校验 |
| 4. 公网部署检查 | 未执行 | 无公网实例验收记录 |
| 5. 发行资产 | 已执行 | 2026-11-27 核对 13 个 Release 资产：12 个在 `SHA256SUMS` 内且 SHA-256 一致（`SHA256SUMS` 自身不自我登记）；`OCI_IMAGE` 与工作流记录的索引摘要一致 |
| 6. 镜像 | 已执行 | `v0.2.25` 发布工作流中的 `build-image` 成功完成双架构构建、冒烟与 Trivy 扫描 |
| 7. 按标签安装 | 未执行 | 无按标签抽测记录 |
| 8. 发布后核对 | 已执行 | Release 已发布且非草稿，资产可下载；回滚路径见[部署与升级](deploy/common.md) |
| 9. 文档与截图 | 已执行 | 2026-11-27 用 v0.2.25 二进制重拍 `docs/assets/` 下 7 张截图（此前仍是 v0.2.18），README 与 `docs/deploy/README.md` 的版本标注同步更新；重拍脚本为 `npm run screenshots` |

v0.2.20、v0.2.21、v0.2.22 的发行说明当时由同一份文件改名产生，没有留下独立记录；对应说明已在 `docs/releases/` 补齐（v0.2.22 仍保留当时的正文）。

后续版本在发布时追加同格式的表格。发布工作流在缺少 `docs/releases/vX.Y.Z.md` 时会自动生成说明（`--generate-notes`），但清单仍要求人工确认说明内容对应该版本。
