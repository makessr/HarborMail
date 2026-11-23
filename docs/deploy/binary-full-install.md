# Linux 二进制全量版部署

适合已有原生 MySQL、Redis、Rspamd 和 ClamAV 的站点。 [所有方案](README.md) · [本方案升级](binary-full-upgrade.md)

1. 按[二进制标准版第 1、2 步](binary-standard-install.md)装好 Harbor Mail，设置 MySQL 与 Redis。暂缓启动 Harbor Mail。
2. 按[Linux 原生扫描部署](../spam-and-antivirus.md#linux-原生扫描部署)接通 Rspamd 与 ClamAV，等待病毒库加载完成。Harbor Mail 调用 Rspamd，由 Rspamd 调用 ClamAV。本文示例使用回环地址。
3. 在 `/etc/harbormail/harbormail.env` 中设置以下内容。

~~~dotenv
HARBOR_RSPAMD_URL=http://127.0.0.1:11333/checkv2
HARBOR_RSPAMD_TIMEOUT_SECONDS=15
HARBOR_VIRUS_ACTION=quarantine
HARBOR_SCAN_FAILURE=defer
~~~

4. [启动 systemd 服务](common.md#启动-systemd-服务)，[登录并试发](common.md#打开后台并试发)。在后台「安全 → 公网部署安全检查」执行来信扫描检查。默认扫描故障返回 SMTP 451，病毒命中进入「隔离邮件」。

[接入公网](../quick-start.md#接入公网) · [扫描策略与排障](../spam-and-antivirus.md)
