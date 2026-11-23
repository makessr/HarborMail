# Linux 二进制标准版升级

适用于由 systemd 运行并连接 MySQL、Redis 的实例。 [所有方案](README.md) · [本方案部署](binary-standard-install.md)

1. 阅读[目标版本说明](https://github.com/makessr/HarborMail/releases/latest)，[下载并校验新二进制](common.md#下载同版二进制)。
2. [备份 MySQL](../../ops/README.md#mysql)。同时备份 `/etc/harbormail`、`/var/lib/harbormail`、systemd 服务和 drop-in，保留登录、DKIM 和 TLS 凭据。外部 MySQL 可以用供应商快照或从受控主机做转储。缓存 Redis 保留原地址与命名空间。
3. [替换二进制](common.md#二进制替换与回退)。共享 MySQL 时，先停止旧版写入。服务启动后核对版本、`/healthz`、旧邮件与实际收发。

[数据库恢复与回滚](../../ops/README.md#切换与回滚)
