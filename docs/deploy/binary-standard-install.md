# Linux 二进制标准版部署

使用 systemd 管理 Harbor Mail，接入已有的 MySQL 8.4 和 Redis。 [所有方案](README.md) · [本方案升级](binary-standard-upgrade.md)

## 1. 安装程序

按[下载并安装二进制](common.md#下载并安装二进制)放好程序、服务和首个管理员。先完成下面的 MySQL 与 Redis 配置，再启动 Harbor Mail。

## 2. 连接 MySQL 和 Redis

为 Harbor Mail 创建专用数据库和应用账号。以同机部署为例，在 MySQL 管理会话中执行，先替换示例口令。

~~~sql
CREATE DATABASE harbor_mail CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;
CREATE USER 'harbor'@'127.0.0.1' IDENTIFIED BY '换成随机强口令';
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, INDEX, REFERENCES
ON harbor_mail.* TO 'harbor'@'127.0.0.1';
~~~

在 `/etc/harbormail/harbormail.env` 中将数据库类型改为 `HARBOR_DB_DRIVER=mysql`，添加 `HARBOR_REDIS_NAMESPACE=harbor-mail`。同一 Redis 上的不同部署请用不同命名空间。

~~~bash
sudo touch /etc/harbormail/mysql-dsn /etc/harbormail/redis-url
sudo chmod 0600 /etc/harbormail/mysql-dsn /etc/harbormail/redis-url
sudoedit /etc/harbormail/mysql-dsn /etc/harbormail/redis-url
~~~

两个文件各写一行，分别填入实际 DSN 和 Redis URL。例如 `harbor:实际密码@tcp(127.0.0.1:3306)/harbor_mail` 和 `redis://127.0.0.1:6379/0`。远程依赖请参照 [MySQL](../deployment-and-usage.md#使用-mysql)和 [Redis](../deployment-and-usage.md#可选-redis-缓存)的 TLS 设置。

执行 `sudo systemctl edit harbormail.service`，加入以下内容。

~~~ini
[Service]
LoadCredential=mysql-dsn:/etc/harbormail/mysql-dsn
LoadCredential=redis-url:/etc/harbormail/redis-url
Environment=HARBOR_MYSQL_DSN_FILE=%d/mysql-dsn
Environment=HARBOR_REDIS_URL_FILE=%d/redis-url
~~~

## 3. 启动并验收

~~~bash
sudo env HARBOR_MYSQL_DSN_FILE=/etc/harbormail/mysql-dsn /usr/local/bin/harbormail mysql-check
~~~

预检通过后，[启动 systemd 服务](common.md#启动-systemd-服务)，[登录并试发](common.md#打开后台并试发)。在后台「运行监控」确认数据库为 MySQL，再按需[接入公网](../quick-start.md#接入公网)。
