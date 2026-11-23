# Linux 二进制精简版升级

适用于已有 SQLite 和 systemd 的实例。 [所有方案](README.md) · [本方案部署](binary-minimal-install.md)

1. 阅读[目标版本说明](https://github.com/makessr/HarborMail/releases/latest)，[下载并校验新二进制](common.md#下载同版二进制)。
2. 做停机备份。以下使用默认目录；自定义数据路径按实际情况替换，另备份自定义 systemd drop-in 和目录之外的密钥。

~~~bash
(
  set -euo pipefail
  sudo systemctl stop harbormail.service
  backup=$(sudo mktemp -d /var/backups/harbormail-sqlite.XXXXXXXX)
  sudo tar -czf "$backup/state.tar.gz" -C / etc/harbormail var/lib/harbormail
  sudo tar -tzf "$backup/state.tar.gz" >/dev/null
  printf '备份目录：%s\n' "$backup"
)
~~~

3. 备份成功后，[替换二进制](common.md#二进制替换与回退)。备份命令失败时，先恢复旧服务，再处理失败原因。
4. 检查版本、`/healthz` 和旧邮件，试发一封，并保留备份。 [数据恢复与回滚](../../ops/README.md#恢复与校验)
