# 通用操作

[返回方案选择](README.md)。命令面向 Linux。示例中的域名、管理员邮箱和端口按实际安装替换。

## 打开后台并试发

默认只在服务器本机监听 Web 8080。从自己的电脑建立 SSH 隧道，并保持终端开启。

~~~bash
ssh -N -L 8080:127.0.0.1:8080 用户名@服务器IP
~~~

打开 http://127.0.0.1:8080/admin，用部署时的管理员邮箱登录。进入「我的邮箱」，写一封发给自己的邮件，确认它出现在收件箱。电脑上的 8080 已被占用时，将 SSH 命令第一个 8080 和浏览器地址改成同一个空闲端口。

[配置 HTTPS、DNS 和公网收发](../quick-start.md#接入公网)

## 下载同版二进制

新建一个空的下载目录，进入目录后执行。命令从 GitHub 读取正式发行标签。需要固定历史版本时，将 `version` 设置为该发行标签。需要的工具有 Bash、curl、tar 和 sha256sum。

~~~bash
release_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' https://github.com/makessr/HarborMail/releases/latest) &&
version=${release_url##*/} &&
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || exit 1
case "$(uname -m)" in
  x86_64) binary=harbormail-linux-amd64 ;;
  aarch64|arm64) binary=harbormail-linux-arm64 ;;
  *) printf '此架构暂无官方 Linux 二进制\n' >&2; exit 1 ;;
esac
(
  set -euo pipefail
  base="https://github.com/makessr/HarborMail/releases/download/$version"
  curl -fLO "$base/$binary"
  curl -fLO "$base/SHA256SUMS"
  curl -fLO "$base/harbormail-systemd.tar.gz"
  awk -v bin="$binary" '$2==bin || $2=="harbormail-systemd.tar.gz" {print; n++}
    END {if (n!=2) exit 1}' SHA256SUMS |
    sha256sum -c -
  chmod u+x "$binary"
  tar -xzf harbormail-systemd.tar.gz
)
~~~

确认两个文件显示 `OK` 后再继续。三个 Linux 方案下载同一套文件。

## 下载并安装二进制

需要 systemd 251 或更新版本。先[下载同版二进制](#下载同版二进制)，在同一终端的下载目录执行以下命令。输入真实的管理员邮箱和 12–72 字节的密码。

~~~bash
(
  set -euo pipefail
  if sudo test -e /etc/harbormail/accounts.json ||
     sudo test -e /etc/harbormail/harbormail.env ||
     sudo test -e /usr/local/bin/harbormail; then
    printf '已有安装，请使用升级文档。\n' >&2
    exit 1
  fi
  read -rp '管理员邮箱: ' admin
  admin=${admin,,}
  [[ $admin =~ ^[a-z0-9][a-z0-9._%+-]*@[a-z0-9.-]+\.[a-z0-9-]+$ ]]
  read -rsp '管理员密码: ' password
  printf '\n'
  hash=$(printf '%s\n' "$password" | "./$binary" hash-password)
  unset password
  getent passwd harbormail >/dev/null ||
    sudo useradd --system --user-group --home-dir /var/lib/harbormail \
      --shell "$(command -v nologin)" harbormail
  sudo install -d -m 0700 /etc/harbormail
  sudo install -m 0755 "$binary" /usr/local/bin/harbormail
  printf '{"%s":"%s"}\n' "$admin" "$hash" |
    sudo sh -eu -c 'umask 077; set -C; cat > /etc/harbormail/accounts.json'
  sudo install -m 0600 ops/harbormail.env.example /etc/harbormail/harbormail.env
  printf '\nHARBOR_ADMINS=%s\n' "$admin" |
    sudo tee -a /etc/harbormail/harbormail.env >/dev/null
  sudo install -m 0644 ops/harbormail.service /etc/systemd/system/harbormail.service
  sudo systemctl daemon-reload
)
~~~

用 `sudoedit /etc/harbormail/harbormail.env` 将 `HARBOR_DOMAIN` 改为服务器邮件主机名，例如 `mail.example.com`。然后回到所选方案，完成 SQLite、MySQL 或扫描配置。

## 启动 systemd 服务

~~~bash
sudo systemctl daemon-reload
sudo systemctl enable --now harbormail.service
sudo systemctl status harbormail.service --no-pager
curl -fsS http://127.0.0.1:8080/healthz
~~~

健康接口返回 `ok` 后，[打开后台并试发](#打开后台并试发)。失败时查看 `sudo journalctl -u harbormail.service -n 80 --no-pager`。自行调整过监听地址或 TLS 时，检查实际健康地址。

## 备份 Docker MySQL 实例

适用于默认 Docker 标准版和全量版，MySQL 在同一项目内，凭据由初始化脚本生成。在**原项目目录**执行。请先安排短暂维护窗口，自定义覆盖文件和目录外的 TLS、扫描配置也要备份。

~~~bash
(
  set -euo pipefail
  umask 077
  backup=$(mktemp -d "$HOME/harbor-mail-backup.XXXXXXXX")
  harbor=$(sudo docker compose ps -q harbor)
  test -n "$harbor"
  sudo cp -a .env secrets compose.*.yaml "$backup/"
  mkdir "$backup/data"
  sudo docker compose stop harbor
  sudo docker cp "$harbor:/var/lib/harbormail/." "$backup/data/"
  sudo docker compose exec -T mysql sh -eu -c '
    export MYSQL_PWD="$(cat "$MYSQL_ROOT_PASSWORD_FILE")"
    exec mysqldump -uroot --single-transaction --quick --no-tablespaces \
      --set-gtid-purged=OFF --hex-blob --max-allowed-packet=64M "$MYSQL_DATABASE"
  ' > "$backup/mysql.sql"
  test -s "$backup/mysql.sql"
  printf '备份目录：%s\n' "$backup"
)
~~~

备份成功后继续更新。备份失败时先用 `sudo docker compose start harbor` 恢复旧服务。外部数据库、复杂覆盖配置和恢复演练参照[生产运行手册](../../ops/README.md#备份)。

## Docker 保存旧镜像并更新

备份完成后，在**原项目目录**和原来的 Compose 参数下运行。使用 `-p` 或 `-f` 的部署，在每条 `docker compose` 命令保留相同参数。

### 固定摘要的部署

从目标 GitHub Release 下载 `OCI_IMAGE` 和 `SHA256SUMS`，在下载目录执行 `awk '$2=="OCI_IMAGE" {print; found=1} END {if (!found) exit 1}' SHA256SUMS | sha256sum -c -`。记录旧的 `HARBOR_IMAGE`，再将部署目录 `.env` 的 `HARBOR_IMAGE` 改为 `OCI_IMAGE` 的内容。运行 `sudo docker compose pull harbor` 和 `sudo docker compose up -d --no-deps --no-build --wait harbor`，然后按目标版本说明验收。满足数据库回滚条件时，恢复旧摘要并用相同参数重建 Harbor Mail。

### 沿用 latest 的部署

以下命令适用于镜像引用为 `ghcr.io/makessr/harbormail:latest` 的已有部署。

~~~bash
(
  set -euo pipefail
  release_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' https://github.com/makessr/HarborMail/releases/latest)
  version=${release_url##*/}
  [[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]
  harbor=$(sudo docker compose ps -a -q harbor)
  test -n "$harbor"
  old=$(sudo docker inspect -f '{{.Image}}' "$harbor")
  rollback="harbormail-rollback:${old#sha256:}"
  sudo docker tag "$old" "$rollback"
  printf '旧镜像标签：%s\n' "$rollback"
  sudo docker compose pull harbor
  pulled=$(sudo docker run --rm --pull never --network none ghcr.io/makessr/harbormail:latest version)
  if [[ $pulled != "$version" ]]; then
    printf '预期 %s，实际拉取到 %s，请核对目标版本。\n' "$version" "$pulled" >&2
    exit 1
  fi
  sudo docker compose up -d --no-deps --no-build --pull never --wait harbor
  sudo docker compose exec -T harbor harbormail version
)
~~~

要安装特定历史版本，在命令中设定该发行标签。保留输出的旧镜像标签。确认打印预期版本，检查健康接口，再用后台账号验证实际收发。备份时已停止服务且升级未继续时，可按下方步骤恢复旧镜像。

若需回退，先核对[数据库回滚条件](../../ops/README.md#切换与回滚)。数据库仍兼容时，把旧镜像指回本地 `latest`，再重建 Harbor Mail。使用旧卷和原配置。

~~~bash
rollback='填入升级前输出的旧镜像标签'
sudo docker tag "$rollback" ghcr.io/makessr/harbormail:latest &&
  sudo docker compose up -d --no-deps --no-build --pull never --force-recreate --wait harbor
~~~

## 二进制替换与回退

先[下载同版二进制](#下载同版二进制)，完成所选方案的数据库和配置备份。在下载目录、同一终端执行以下命令。自定义安装以服务的 `ExecStart` 和 `EnvironmentFile` 为准。

~~~bash
(
  set -euo pipefail
  backup=$(sudo mktemp -d /var/backups/harbormail-bin.XXXXXXXX)
  sudo cp -p /usr/local/bin/harbormail "$backup/harbormail"
  printf '旧二进制：%s/harbormail\n' "$backup"
  sudo install -m 0755 "$binary" /usr/local/bin/harbormail.next
  sudo systemctl stop harbormail.service
  sudo mv -f /usr/local/bin/harbormail.next /usr/local/bin/harbormail
  sudo systemctl start harbormail.service
  sudo systemctl status harbormail.service --no-pager
  /usr/local/bin/harbormail version
)
~~~

再检查健康接口并验证收发。保留原环境文件和 systemd 配置。只有修改服务单元或 drop-in 时才执行 `sudo systemctl daemon-reload`。

若需回退，先核对[数据库回滚条件](../../ops/README.md#切换与回滚)。确认兼容后，停止服务，将旧二进制装回原路径，再启动并检查版本。需要恢复数据库时使用隔离恢复流程，并核对升级后已接收的邮件。
