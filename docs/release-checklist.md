# 发布检查清单

正式版本发布前，在候选提交上逐项执行，并在下方「执行记录」中写入该版本的实际结果和证据。检查项只有三种状态：**已执行**（附工作流链接、命令输出或截图）、**未执行**（写原因）、**本轮不适用**。没有执行过的项目不要勾选，也不要用复选框代替证据。

检查项里标注「自动」的由 CI 在候选提交上执行，人工只需确认运行结论；标注「人工」的必须在真实部署或本机演练中完成。

## 检查项

1. **主干回归（自动）**：`go test -race ./... -count=1`、独立 SMTP/IMAP/POP3 客户端测试、三个解析器模糊测试。见 `.github/workflows/verify.yaml` 的 `concurrency-and-parsing`。
2. **数据库演练（人工）**：合成数据下，SQLite 与 MySQL 各完成一次升级、回滚和备份恢复。共享 MySQL 的实例还要验证停写切换。`.github/scripts/test-upgrade-drill.mjs`（由 `.github/workflows/drill.yaml` 触发）可复现这条路径：旧版本写入数据 → 新版本升级 → 备份 → 恢复 → 旧版本回滚。
3. **站内路径（自动）**：Web、SMTP、IMAP、POP3 的站内收发由上面的自动化覆盖，并通过 `go test ./...` 在 SQLite 与隔离 MySQL 上各跑一遍。
4. **公网部署检查（人工）**：TLS 证书与续期、DNS 与 PTR、防火墙端口、可信代理、Secure Cookie、后台两步验证。管理后台「安全」页的检查项可作为清单。
5. **发行资产（自动 + 人工核对）**：Linux 与 Windows 的 amd64、arm64 二进制版本号正确；`SHA256SUMS` 覆盖 Release 全部资产且摘要一致；`OCI_IMAGE` 与 GHCR 上 `latest` 的索引摘要一致；`docs/releases/vX.Y.Z.md` 存在且内容对应该版本。
6. **镜像（自动）**：双架构构建、按架构冒烟（`version`、许可证文件、`hash-password`）、Trivy 对 HIGH/CRITICAL 校验「无可修复项」。见 `.github/actions/build-image/action.yaml`。
7. **按标签安装（人工）**：抽测 `vX.Y.Z` 标签下的 Compose 与 systemd 安装包，确认安装后版本号与标签一致。`.github/workflows/drill.yaml` 的 `tag-install` 作业会下载该标签的发行包，校验 `SHA256SUMS`、按 systemd 安装并检查 `/healthz`，同时校验该标签的 Compose 包可解析。
8. **发布后核对（人工）**：检查下载资产和镜像摘要可访问，记录本版本的回滚路径和兼容性条件。
9. **文档与截图（人工）**：README 与部署文档里的界面截图对应当前版本，版本标注与本次发布一致；界面有改动时用 `npm run screenshots` 重拍并替换 `docs/assets/*.webp`。

## 执行记录

### v0.2.25（2026-11-23 发布）

| 检查项 | 状态 | 证据 |
| --- | --- | --- |
| 1. 主干回归 | 已执行 | Verify 工作流 [45180524527](https://github.com/makessr/HarborMail/actions/runs/45180524527)（main）与 [45180523185](https://github.com/makessr/HarborMail/actions/runs/45180523185)（v0.2.25）均成功，含 race、协议客户端、模糊测试；提交 `3502a39` 的 [45938311990](https://github.com/makessr/HarborMail/actions/runs/45938311990) 六个作业全绿（race、协议客户端、模糊测试、MySQL 8.4、Chromium、systemd 冒烟、镜像配置校验）；2026-11-27 本地 `go test ./... -count=1` 通过 |
| 2. 数据库演练 | 已执行 | 2026-11-27 [45938850812](https://github.com/makessr/HarborMail/actions/runs/45938850812) 的 `upgrade-drill`（sqlite、mysql）均通过：v0.2.24 写入 3 封邮件 → 升级到 main 后数据与主题不变、`letters` 表未被收窄并新增 `password_recovery_codes` 表 → 备份 → 恢复到备份后旧版本可读；本机 Windows SQLite 同流程通过 |
| 3. 站内路径 | 已执行 | 同第 1 项，`verify` 作业在隔离 MySQL 与 SQLite 上各跑一遍；含 Chromium 网页回归与 Compose/Prometheus 配置校验 |
| 4. 公网部署检查 | 未执行 | 无公网实例：该检查需要一台可对外访问的主机（域名、TLS 证书与续期、PTR、防火墙端口、可信代理）才能执行 |
| 5. 发行资产 | 已执行 | 2026-11-27 核对 13 个 Release 资产：12 个在 `SHA256SUMS` 内且 SHA-256 一致（`SHA256SUMS` 自身不自我登记）；`OCI_IMAGE` 与工作流记录的索引摘要一致 |
| 6. 镜像 | 已执行 | `v0.2.25` 发布工作流中的 `build-image` 成功完成双架构构建、冒烟与 Trivy 扫描 |
| 7. 按标签安装 | 已执行 | 2026-11-27 [45938850812](https://github.com/makessr/HarborMail/actions/runs/45938850812) 的 `tag-install` 作业：下载 v0.2.25 的 systemd 包与 `harbormail-linux-amd64` 并 `sha256sum --check` 通过，`version` 与标签一致，装成 systemd 服务后 `/healthz` 返回 ok、`systemctl is-active` 为 active；v0.2.25 标签的 Compose 包 `docker compose config` 全部通过 |
| 8. 发布后核对 | 已执行 | Release 已发布且非草稿，资产可下载；回滚路径见[部署与升级](deploy/common.md) |
| 9. 文档与截图 | 已执行 | 2026-11-27 用 v0.2.25 二进制重拍 `docs/assets/` 下 7 张截图（此前仍是 v0.2.18），README 与 `docs/deploy/README.md` 的版本标注同步更新；重拍脚本为 `npm run screenshots` |

v0.2.20、v0.2.21、v0.2.22 的发行说明当时由同一份文件改名产生，没有留下独立记录；对应说明已在 `docs/releases/` 补齐（v0.2.22 仍保留当时的正文）。

两项演练可以在 GitHub Actions 上重跑，不需要本机 Docker 或 systemd：

```bash
gh workflow run drill.yaml -f from=v0.2.24 -f to=main -f tag=v0.2.25
```

`upgrade-drill` 作业覆盖数据库升级、备份和回滚（SQLite 与 MySQL 8.4 各一份），`tag-install` 作业覆盖按标签安装 systemd 包并校验 Compose 包。公网部署检查没有自动化路径，必须在真实主机上人工完成。

v0.2.25 的 systemd 包早于本次改动，因此不包含 `ops/install-compose-maintenance.sh`；下一次发布后该脚本会随包分发，届时可按第 7 项在标签包内找到它。

后续版本在发布时追加同格式的表格。发布工作流在缺少 `docs/releases/vX.Y.Z.md` 时会自动生成说明（`--generate-notes`），但清单仍要求人工确认说明内容对应该版本。
