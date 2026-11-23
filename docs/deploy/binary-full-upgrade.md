# Linux 二进制全量版升级

适用于由 systemd 运行并连接 MySQL、Redis、Rspamd 和 ClamAV 的实例。 [所有方案](README.md) · [本方案部署](binary-full-install.md)

1. 完成[二进制标准版升级的第 1、2 步](binary-standard-upgrade.md)。同时保留 Rspamd、ClamAV 配置和扫描 Redis 数据。病毒库和扫描组件按各自的维护流程更新。
2. [替换二进制](common.md#二进制替换与回退)。保持原有扫描 URL 和故障策略。共享 MySQL 时，先停止旧版写入。
3. 检查版本、`/healthz` 和实际收发。在后台「安全 → 公网部署安全检查」运行来信扫描检查。扫描失败时，先查看 Harbor Mail、Rspamd 和 ClamAV 的日志。

~~~bash
sudo journalctl -u harbormail.service -u rspamd.service -n 80 --no-pager
~~~

[查找扫描服务配置与日志](../spam-and-antivirus.md#linux-原生扫描部署) · [恢复与回滚](../../ops/README.md#切换与回滚)
