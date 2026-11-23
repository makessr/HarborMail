# Docker 精简版升级

适用于已有 SQLite 单实例。 [所有方案](README.md) · [本方案部署](docker-minimal-install.md)

1. 进入原 Compose 项目目录，阅读[目标版本说明](https://github.com/makessr/HarborMail/releases/latest)。保留原 `.env`、`secrets/`、Compose 文件和数据卷。升级无需再运行初始化。
2. 在维护窗口停止 Harbor Mail，备份整个数据目录和部署文件。下例用于默认单文件项目；自定义项目还要备份实际使用的覆盖文件及其他凭据。

~~~bash
(
  set -euo pipefail
  umask 077
  backup=$(mktemp -d "$HOME/harbor-mail-backup.XXXXXXXX")
  harbor=$(sudo docker compose ps -q harbor)
  test -n "$harbor"
  sudo cp -a .env secrets compose.minimal.yaml "$backup/"
  mkdir "$backup/data"
  sudo docker compose stop harbor
  sudo docker cp "$harbor:/var/lib/harbormail/." "$backup/data/"
  printf '备份目录：%s\n' "$backup"
)
~~~

3. 备份成功后，[保存旧镜像并更新](common.md#docker-保存旧镜像并更新)。按原项目名和挂载启动，切勿删除卷。如果停机备份失败，先执行 `sudo docker compose start harbor` 恢复旧服务。
4. 核对目标版本、`/healthz` 和旧邮件，再试发一封。版本启动失败时查看 `sudo docker compose logs --tail=80 harbor`。 [回退条件与操作](common.md#docker-保存旧镜像并更新)
