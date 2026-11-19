# Harbor Mail

<img src="web/harbor-mail-logo.png" alt="" width="72" height="72">

Harbor Mail 是面向 Linux 部署的自建域名邮箱，提供中文 Web 邮箱、管理后台和标准邮件协议。Go 程序内嵌前端资源，支持单二进制或 Docker Compose 部署。

## 功能

| 场景 | 支持内容 |
| --- | --- |
| 网页邮箱 | 多收件人、抄送与密送、附件、草稿自动保存、定时发送与撤销、邮件会话、签名和模板 |
| 邮件整理 | 嵌套文件夹、标签、规则、全文搜索、保存搜索、稍后处理、批量操作及 EML 导入导出 |
| 邮件协议 | SMTP 收信和提交、IMAP（含 IDLE）、POP3、应用密码及 TLS |
| 管理后台 | 多域名、域管理员、邮箱与别名管理、容量配额、投递队列、运行监控、审计和管理员 MFA |
| 数据与部署 | SQLite、MySQL、可选 Redis 后台缓存、数据库迁移、备份与恢复工具 |

## Linux 单二进制启动

在有 Go 1.27.4 或更新版本的构建机上执行：

```bash
git clone https://github.com/makessr/HarborMail.git
cd HarborMail
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o harbormail .
```

ARM64 服务器把 `GOARCH=amd64` 改为 `GOARCH=arm64`。将生成的 `harbormail` 复制到 Linux 服务器，按[原生启动说明](docs/deployment-and-usage.md#linux-单二进制)创建首个账号，然后运行 `./harbormail`。默认使用 SQLite，运行服务器只需要二进制、账号配置和可写的数据目录。Go 仅用于构建。

## Linux Docker Compose（MySQL + Redis）

在 Linux amd64 主机的项目目录，按[部署指南](docs/deployment-and-usage.md#docker-compose-部署)准备首次登录与数据库凭据。Harbor Mail 从 GHCR 拉取，MySQL 和 Redis 使用各自的官方镜像。

```bash
docker compose -f compose.yaml -f compose.mysql.yaml -f compose.registry.yaml up -d --no-build --wait
docker compose -f compose.yaml -f compose.mysql.yaml -f compose.registry.yaml ps
```

Harbor Mail 镜像为 `ghcr.io/makessr/harbormail:latest`。[镜像包页面](https://github.com/users/makessr/packages/container/package/harbormail)

打开 [Web 邮箱](http://127.0.0.1:8080/?view=mail)，用 `hello@harbor.test` 和刚设置的密码登录。Web 和 SMTP 默认监听主机回环端口 `8080` 和 `2525`。Redis 用于后台统计缓存，服务不可用时回源 MySQL。

## 开发与测试

在源码目录运行 `go test ./...`。Windows 可用于本地开发和构建 Linux 目标，详见 [Windows 开发说明](docs/deployment-and-usage.md#windows-开发)。

## 文档与验收

- [部署与使用指南](docs/deployment-and-usage.md)涵盖配置、邮件客户端、Web 功能和数据库迁移。
- [生产运行手册](ops/README.md)涵盖 Linux 部署、站点验收、监控告警和备份恢复。
- [接入真实域名](docs/deployment-and-usage.md#接入真实域名)涵盖 DNS、邮件 TLS、中继和公网接入。

已在本地 Docker 验证 MySQL 与 Redis 部署、SMTP 收信、Redis 停机回源及双实例网页同步。正式域名的公网收发、TLS 续签和灾难恢复需要在目标环境验收。
