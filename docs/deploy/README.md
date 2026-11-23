# Harbor Mail 部署与升级

适用于 [Harbor Mail v0.2.23](https://github.com/makessr/HarborMail/releases/tag/v0.2.23)。新用户建议选 **Docker 标准版**。只想先跑通邮箱，选**精简版**。需要来信反垃圾和病毒隔离，选**全量版**。

| 安装方式 | 方案 | 包含组件 | 首次部署 | 已有实例 |
| --- | --- | --- | --- | --- |
| Docker | 精简版 | Harbor Mail、SQLite | [部署](docker-minimal-install.md) | [升级](docker-minimal-upgrade.md) |
| Docker | **标准版（推荐）** | Harbor Mail、MySQL、Redis | [部署](docker-standard-install.md) | [升级](docker-standard-upgrade.md) |
| Docker | 全量版 | 标准版加 Rspamd、ClamAV 和扫描用 Redis | [部署](docker-full-install.md) | [升级](docker-full-upgrade.md) |
| Linux 二进制 | 精简版 | Harbor Mail、SQLite | [部署](binary-minimal-install.md) | [升级](binary-minimal-upgrade.md) |
| Linux 二进制 | 标准版 | Harbor Mail，接入现有 MySQL、Redis | [部署](binary-standard-install.md) | [升级](binary-standard-upgrade.md) |
| Linux 二进制 | 全量版 | 标准版加现有 Rspamd、ClamAV | [部署](binary-full-install.md) | [升级](binary-full-upgrade.md) |

Docker 会启动所选方案的配套服务。Linux 三种方案使用同一个发行二进制，区别在于连接的组件。项目原有的「极简版」对应表中的「精简版」。

## 跑通以后

1. [打开后台并试发一封站内邮件](common.md#打开后台并试发)
2. [接入 HTTPS、域名和公网邮件](../quick-start.md#接入公网)
3. [从外网验收收发](../quick-start.md#上线验收与日常命令)

![Harbor Mail 管理后台概览，包含系统状态和关于 Harbor Mail 的入口](../assets/admin-overview.webp)

截图来自 v0.2.18 Docker 标准版，使用演示数据，用于定位管理后台。另可查看[网页邮箱](../assets/webmail.webp)和 [DNS 记录界面](../assets/admin-dns.webp)。
