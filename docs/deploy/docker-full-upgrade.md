# Docker 全量版升级

适用于已有 MySQL、Redis、Rspamd、ClamAV 和扫描用 Redis 的实例。 [所有方案](README.md) · [本方案部署](docker-full-install.md)

1. 在原项目目录完成 [Docker 标准版升级的第 1 步](docker-standard-upgrade.md)。同时保留扫描配置、Rspamd 和扫描 Redis 的数据卷、ClamAV 病毒库卷。只升级 Harbor Mail 时，扫描组件继续使用原镜像和卷。
2. [保存旧镜像并更新](common.md#docker-保存旧镜像并更新)。共享 MySQL 时，先停止旧版写入。
3. 检查 Harbor Mail 版本、`/healthz` 和实际收发。在后台「安全 → 公网部署安全检查」运行来信扫描检查。若收信出现等待重试，先查看扫描组件的状态和日志。

~~~bash
sudo docker compose ps
sudo docker compose logs --tail=80 harbor rspamd clamav
~~~

[扫描排障](../spam-and-antivirus.md) · [回退条件与操作](common.md#docker-保存旧镜像并更新)
