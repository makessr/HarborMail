# Harbor Mail

<img src="web/harbor-mail-logo.png" alt="" width="72" height="72">

Harbor Mail 是用 Go 编写的自建域名邮箱，提供中文 Web 邮箱、管理后台和标准邮件协议。前端资源内嵌在程序中，部署时用一个可执行文件或 Docker Compose 即可启动。

## 功能

| 场景 | 支持内容 |
| --- | --- |
| 网页邮箱 | 多收件人、抄送与密送、附件、草稿自动保存、定时发送与撤销、邮件会话、签名和模板 |
| 邮件整理 | 嵌套文件夹、标签、规则、全文搜索、保存搜索、稍后处理、批量操作及 EML 导入导出 |
| 邮件协议 | SMTP 收信和提交、IMAP（含 IDLE）、POP3、应用密码及 TLS |
| 管理后台 | 多域名、域管理员、邮箱与别名管理、容量配额、投递队列、运行监控、审计和管理员 MFA |
| 数据与部署 | SQLite、MySQL、可选 Redis 后台缓存、数据库迁移、备份与恢复工具 |

## Docker 快速启动

需要 Docker Engine、BuildKit 和 Docker Compose。命令示例使用 PowerShell 7，Linux 凭据初始化见[生产运行手册](ops/README.md#首次启动与上线验收)。

```powershell
git clone https://github.com/makessr/HarborMail.git
Set-Location HarborMail
docker compose -f compose.yaml build harbor
if ($LASTEXITCODE -ne 0) { throw 'Harbor Mail 镜像构建失败' }
$null = New-Item -ItemType Directory -Force secrets
$hash = Read-Host '输入邮箱密码（12–72 字节）' -MaskInput |
  docker run --rm -i harbormail:local hash-password
if ($LASTEXITCODE -ne 0) { throw '密码哈希生成失败' }
@{ 'hello@harbor.test' = $hash } | ConvertTo-Json -Compress |
  Set-Content -Encoding utf8NoBOM -NoNewline ./secrets/accounts.json
docker compose -f compose.yaml up -d --wait
```

打开 [Web 邮箱](http://127.0.0.1:8080/?view=mail)，用 `hello@harbor.test` 和刚设置的密码登录。默认使用 SQLite，Web 和 SMTP 分别绑定在主机回环端口 `8080` 和 `2525`。

需要管理后台时，在 `.env` 添加 `HARBOR_ADMINS=hello@harbor.test`，重新运行 Compose，然后访问 [管理后台](http://127.0.0.1:8080/admin)。

### MySQL 和 Redis

先按[部署指南](docs/deployment-and-usage.md#启动-mysql-与-redis)生成 MySQL 凭据，再运行：

```sh
docker compose -f compose.yaml -f compose.mysql.yaml up -d --build --wait
docker compose -f compose.yaml -f compose.mysql.yaml ps
```

该配置同时启动 MySQL 8.4 与 Redis。Redis 用于后台统计缓存，服务不可用时回源 MySQL。网页邮箱支持共享 MySQL 的多实例同步。

## 从源码构建

需要 Go 1.27.4 或更新版本。

```sh
go build -o harbormail .
go test ./...
```

Windows 构建时可使用 `go build -o harbormail.exe .`。[配置账号并运行](docs/deployment-and-usage.md#本地启动)

## 文档与验收

- [部署与使用指南](docs/deployment-and-usage.md)涵盖配置、邮件客户端、Web 功能和数据库迁移。
- [生产运行手册](ops/README.md)涵盖 Linux 部署、站点验收、监控告警和备份恢复。
- [接入真实域名](docs/deployment-and-usage.md#接入真实域名)涵盖 DNS、邮件 TLS、中继和公网接入。

已在本地 Docker 验证 MySQL 与 Redis 部署、SMTP 收信、Redis 停机回源及双实例网页同步。正式域名的公网收发、TLS 续签和灾难恢复需要在目标环境验收。
