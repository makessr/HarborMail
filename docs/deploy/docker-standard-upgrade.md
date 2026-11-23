# Docker 标准版升级

适用于已有 MySQL 和 Redis 的实例。 [所有方案](README.md) · [本方案部署](docker-standard-install.md)

1. 在原 Compose 项目目录阅读[目标版本说明](https://github.com/makessr/HarborMail/releases/latest)。用[默认 MySQL Compose 备份步骤](common.md#备份-docker-mysql-实例)备份邮件、配置、凭据和 Harbor 数据。外部 MySQL 参照[生产备份手册](../../ops/README.md#mysql)。
2. [保存旧镜像并更新](common.md#docker-保存旧镜像并更新)。只更新 `harbor`，保留 MySQL 和 Redis。多个实例共享一个库时，需在备份前统一停止旧版写入。
3. 核对版本、`/healthz`、后台数据库状态和旧邮件，验证实际收发。升级中若遇到数据库初始化失败，先看 `sudo docker compose logs --tail=80 harbor`。

若你的项目最后加载 `compose.server.yaml`，由同项目的 Docker MySQL 提供数据库，可选用[升级脚本](../../ops/README.md#使用-latest-升级单实例-mysql-compose)。它会核对预期版本、备份并在新容器验证失败时恢复旧镜像。
