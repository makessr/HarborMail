# Linux 二进制精简版部署

适合单机和个人邮箱。使用内置 SQLite。 [所有方案](README.md) · [本方案升级](binary-minimal-upgrade.md)

1. 按[下载并安装二进制](common.md#下载并安装二进制)装好程序、systemd 服务和首个管理员。
2. 打开 `/etc/harbormail/harbormail.env`，确认以下两项。

~~~dotenv
HARBOR_DB_DRIVER=sqlite
HARBOR_DB=/var/lib/harbormail/harbor.db
~~~

3. 执行[启动 systemd 服务](common.md#启动-systemd-服务)。`/healthz` 返回 `ok` 后，[登录并试发](common.md#打开后台并试发)。

系统会自动创建 SQLite 文件和数据目录。准备上线时按[公网接入](../quick-start.md#接入公网)配置域名和 TLS。
