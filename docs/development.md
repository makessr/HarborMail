# 开发与验证

## 收信、发信与邮箱写入

- **启动**。程序从 `cmd/harbormail` 进入 `internal/server/server.go` 的 `Run`，处理命令和部署配置。 `configureMailApp` 校验初始账号、邮件大小和 SMTP 资源预算，随后打开数据库并启动协议监听器与后台任务。
- **收信**。 `delivery.Rcpt` 解析本地收件地址和别名。 `delivery.Data` 读取原始 MIME、执行扫描，再在同一事务内写入已接受的收件人邮箱。 `commitMailbox` 提交共享 MySQL 邮箱修订号并通知本地订阅者。
- **发信**。Web 的 `submitWebMail` 和 SMTP 的 `storeSubmission` 校验发件身份、目标和额度。需要入队时，同一事务保存邮件及信封信息。队列用租约领取任务，记录每个外部收件人的进度。远端最终确认未知时保留待确认状态，避免自动重复发送。
- **邮箱写入**。SQLite 与 MySQL 共用主要业务路径。 `lockIMAPUIDs` 保护现有 IMAP UID 分配及与之关联的邮箱成员变动。修改其调用方时，先确认它是否承担文件夹、草稿、账号状态或权限的并发约束。 `pop_octets` 与 `raw` 需要一起写入或复制。历史行的空值由后台任务回填，回填期间按原始邮件计算。

SMTP 收信在事务外准备 MIME 索引元数据，并在多个收件人间复用。IMAP 删除标记依靠带邮箱、文件夹和 UID 条件的邮件行更新，与 UID 分配可以并行。草稿追加的原始邮件元数据也在事务外准备。新建和重命名文件夹由邮箱访问行串行化。标签删除与标签规则写入锁定标签行。文件夹删除仍需协调邮件移动和规则引用。

当前每个数据库使用一个 UID 分配器。若要按邮箱文件夹拆分，需保留已公开的 UID 和 UIDVALIDITY，为每个文件夹设置高于既有 UID 的 UIDNEXT，并把唯一约束改为按文件夹判断。迁移还要验证 COPY、MOVE、删信后的 UID 单调性，避免新旧实例并行时同时分配。应先隔离和验证账户状态、草稿和文件夹操作现在依赖的事务约束，再替换分配器。

## 包结构

`internal/mailmime` 解析邮件正文与内嵌图片，并提供 Web 阅读、打印和翻译共用的 HTML 清理策略。`internal/mailqueue` 通过存储接口获取可投递任务、恢复租约和确定下次运行时间。投递状态事务、SMTP 路由和邮箱通知由 `internal/server` 的实现负责。

## 本地回归

- 执行 `go test ./...` 和 `go vet ./...`。提供专用、可丢弃的 `HARBOR_TEST_MYSQL_DSN` 可启用 MySQL 分支。 `TestMySQLMailboxWriteContention` 使用共享数据库上的两个应用实例和六个独立邮箱，对比持有全局锁与只读快照时 SMTP 收信、网页草稿和 IMAP 的 P95 与 InnoDB 行锁等待。该测试也检查翻译缓存、Web Push 初始化及已有 UID 的 IMAP 快照和删除标记能否在另一实例持有全局锁时完成。
- 2026-11-23 本机 Docker Desktop MySQL 8.4 基线，每类操作 40 个样本。已有 UID 的只读快照模式下，收信、网页草稿、IMAP 快照 P95 分别为 37.1、37.3、14.3 ms，记录到 79 次 InnoDB 行锁等待。本次复测分别为 37.4、36.7、14.0 ms，等待仍为 79 次。该组数据用于后续比较。删除标记脱离全局锁由单独断言验证。
- 执行 `npm ci`、`npx playwright install chromium` 和 `npm run test:web` 运行浏览器回归。脚本使用临时 SQLite、回环端口和合成账号，验证登录、草稿自动保存与刷新后恢复、定时状态、取消定时及站内投递。
- 修改 Tailwind 样式后执行 `npm run build:ui`，并提交对应的 `web/harbor-ui.css` 变化。

CI 在主分支执行 Go 竞态检测及 MIME、POP3 TOP、EML 导入的模糊测试。浏览器回归还覆盖管理员 MFA、账号权限与 HTML 邮件清理。独立客户端矩阵使用 Python 的 `smtplib`、`imaplib` 与 `poplib`，可在 Linux 构建后二进制上运行 `python3 .github/scripts/test-mail-clients.py ./harbormail`。

## 发版前升级演练

在独立数据卷、回环端口和合成账号上，用上一版镜像的实际摘要建立 SQLite 与 MySQL 数据。先投递一封基线邮件，再停止旧实例并启动候选版本。核对原有邮件可读，验证 SMTP 收信、Web 站内发信及 POP3S 的 `STAT` 和 `LIST`。再停止候选版本，用同一套数据启动旧镜像，确认保留升级阶段的邮件并重复收发。最后再次升级，核对回滚阶段新增的邮件也能读到。

这条演练验证顺序切换版本。旧版本的草稿更新不会同步维护新版的 POP3 尺寸字段，升级使用共享 MySQL 时应先停写旧实例，避免新旧实例并行更新同一邮箱。涉及生产备份恢复和切换时按[生产运行手册](../ops/README.md#切换与回滚)执行。
