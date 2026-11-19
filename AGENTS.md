# Harbor Mail 项目约定

- Harbor Mail 是 Go 自建邮箱。对邮件存储、迁移或持久化查询的改动要考虑 SQLite 和 MySQL 两种受支持的部署。
- Linux 是部署与运行目标，既支持单二进制也支持 Docker Compose。Windows 用于本地开发。
- 部署入口在 [README.md](README.md)，生产挂载、TLS、备份和验收见 [生产运行手册](ops/README.md)。
- Docker 演练使用独立项目、合成账号和专用数据卷。区分本机验证、目标环境验收与生产操作。
- 需要执行 Docker 部署或跨实例同步验收时，可调用 `$harbor-mail-docker-validation`。
