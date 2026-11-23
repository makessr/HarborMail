# 第一次部署 Harbor Mail

本教程用 `example.com` 作为邮箱域名，`mail.example.com` 作为网页和邮件主机，`admin@example.com` 作为第一个管理员。操作时全部换成你自己的值。先跑通站内邮件，再接入公网。

要直接查看单个方案的部署或升级步骤，请打开[部署与升级指南](deploy/README.md)。

## 选一种安装方式

| 方案 | 适合谁 | 包含组件 |
| --- | --- | --- |
| Docker 标准版（推荐） | 第一次部署 | Harbor Mail、MySQL、Redis |
| Docker 全量版 | 需要来信扫描和病毒隔离 | 标准版加 Rspamd、ClamAV |
| Docker 极简版 | 单机试用或小规模使用 | Harbor Mail、SQLite |
| Linux 单二进制 | 已有原生服务，或想用 systemd 管理 | 单个可执行文件，按需接入外部服务 |

## 路线 A · Docker

在 **Linux 服务器终端**确认 `docker compose version` 可用。全量版建议使用 Compose 2.23.1 或更新版本。[安装 Docker Engine](https://docs.docker.com/engine/install/) · [安装 Compose 插件](https://docs.docker.com/compose/install/linux/)

用能运行 Docker 并设置凭据文件权限的账号执行：

```bash
release_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' https://github.com/makessr/HarborMail/releases/latest) &&
version=${release_url##*/} &&
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] &&
git clone --branch "$version" --depth 1 https://github.com/makessr/HarborMail.git &&
cd HarborMail &&
sudo bash ops/init-docker.sh standard &&
  sudo docker compose config --quiet &&
  sudo docker compose up -d --no-build --wait --wait-timeout 900
sudo docker compose ps
```

初始化时输入 `admin@example.com` 和准备好的密码。留空才会使用演示邮箱 `hello@harbor.test`。脚本会生成 `.env` 和 `secrets/`，并选用 `compose.standard.yaml`。安装全量或极简版时，把 `standard` 换成 `full` 或 `minimal`，分别使用 `compose.full.yaml` 或 `compose.minimal.yaml`。三个方案只选一个。全量版首次加载病毒库可能需要几分钟。

使用 1Panel 的 Compose 页面时，选用同一版本对应的单文件，并把初始化生成的 `.env` 与 `secrets/` 放在项目目录。已经从终端启动的实例可继续用原目录管理。MySQL 和 Redis 的端口默认只在 Compose 网络内使用。[Docker 详细配置](deployment-and-usage.md#docker-compose-部署)

### 选择数据存放位置

默认使用 Docker 命名卷。新装时想在主机上直接管理目录，可以在第一次 `up` 前把下面的路径填进 `.env`。极简版只需要第一项。

```dotenv
HARBOR_DATA_PATH=/srv/harbor-mail/data
HARBOR_MYSQL_DATA_PATH=/srv/harbor-mail/mysql
```

预先创建目录。Harbor 数据目录须允许容器的 UID/GID `10001:10001` 写入。MySQL 目录须满足官方镜像的写入权限。全量版还可设置 `HARBOR_RSPAMD_DATA_PATH`、`HARBOR_RSPAMD_REDIS_DATA_PATH` 和 `HARBOR_CLAMAV_DATA_PATH`，未设置的组件继续用命名卷。已有实例更换位置前，应先备份并把原数据迁移到目标目录。 [生产存储与迁移](../ops/README.md#存储上限)

## 路线 B · Linux 单二进制

在服务器执行 `uname -m`。`x86_64` 选 `harbormail-linux-amd64`，`aarch64` 选 `harbormail-linux-arm64`。同时下载同版本的 `harbormail-systemd.tar.gz` 和 `SHA256SUMS`。长期运行的服务模板需要 systemd 251 或更新版本。[下载 GitHub Release](https://github.com/makessr/HarborMail/releases)

以下以 x86_64 和首次安装 SQLite 为例。在下载目录运行：

```bash
(
  set -e
  sha256sum --ignore-missing -c SHA256SUMS
  install -m 0755 harbormail-linux-amd64 ./harbormail
  tar -xzf harbormail-systemd.tar.gz
  umask 077
  mkdir -p secrets
  set -o noclobber
  read -rsp '输入邮箱密码（12–72 字节）: ' password
  printf '\n'
  hash=$(printf '%s\n' "$password" | ./harbormail hash-password)
  unset password
  printf '{"admin@example.com":"%s"}\n' "$hash" > secrets/accounts.json
)
```

确认上述命令成功后，安装服务和凭据。以下命令适用于新安装目录。

```bash
getent passwd harbormail >/dev/null ||
  sudo useradd --system --user-group --home-dir /var/lib/harbormail --shell "$(command -v nologin)" harbormail
sudo install -d -m 0700 /etc/harbormail
sudo install -m 0755 ./harbormail /usr/local/bin/harbormail
sudo install -m 0600 secrets/accounts.json /etc/harbormail/accounts.json
sudo install -m 0600 ops/harbormail.env.example /etc/harbormail/harbormail.env
sudo install -m 0644 ops/harbormail.service /etc/systemd/system/harbormail.service
sudoedit /etc/harbormail/harbormail.env
```

在环境文件里新增 `HARBOR_ADMINS=admin@example.com`，把已有的 `HARBOR_DOMAIN` 改为 `mail.example.com`，保留 SQLite 默认设置。单二进制标准版需要接入已有 MySQL、Redis，全量版再接入 Rspamd 和 ClamAV。[单二进制标准版与全量版](../ops/README.md#linux-单二进制-systemd-部署)

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now harbormail.service
sudo systemctl status harbormail.service --no-pager
curl -fsS http://127.0.0.1:8080/healthz
```

## 第一次登录并测试站内收发

从**你自己的电脑**终端运行 `ssh -N -L 8080:127.0.0.1:8080 用户名@服务器IP`，保持终端开启。打开浏览器访问 `http://127.0.0.1:8080/admin`，用刚设置的管理员登录。切换到「我的邮箱」，给自己发一封邮件，确认它出现在收件箱。

要移除安装时的默认管理员，先在「账号 → 权限与访问」把另一个邮箱设为全站管理员，并用该账号在另一浏览器登录后台。回到原管理员，在「账号」打开自己的邮箱，点击「交接并回收」。原账号会退出，邮件和设置仍保留。接手的管理员可在「账号 → 邮箱管理」恢复原账号，或输入完整邮箱地址和自己的主邮箱密码永久删除。交接后仍可在后台管理全站管理员权限。

在「域名」点击「删除域名」。若还有主邮箱或别名，提示框会提供处理入口。地址处理完成后即可删除已登记的域名，自动生成的域名会随最后一个地址移除。公网 MX、SPF、DKIM 和其他 DNS 记录在域名服务商处维护。

## 接入公网

1. 先为 `mail.example.com` 添加指向公网 IPv4 的 A 记录。用 Cloudflare 时选「仅 DNS」。在 1Panel 创建 HTTPS 反向代理。若代理可访问宿主机回环地址，回源填 `http://127.0.0.1:8080`。启用 HTTPS 后设置 `HARBOR_SECURE_COOKIE=true` 并应用配置。要由 Harbor Mail 申请证书，可使用 [Cloudflare DNS-01](deployment-and-usage.md#cloudflare-dns-01)。
2. 在后台「域名 → DNS 记录」确认邮箱域是 `example.com`。按下表发布 DNS 记录，到后台检查并启用 DKIM。
3. 让云安全组和主机防火墙放行入站 TCP 25。Docker 在 `.env` 设置 `HARBOR_SMTP_BIND=0.0.0.0` 和 `HARBOR_SMTP_PORT=25`。单二进制设置 `HARBOR_SMTP=0.0.0.0:25`，并给 systemd 服务授予绑定低端口的权限。确认 25 端口可用后再切换有旧邮箱服务的域名 MX。[低端口和 TLS](../ops/README.md#tls-与版本升级)
4. 原生 MX 直投在新版本中默认开启。将 `HARBOR_DIRECT_MX_HELO` 设为 `mail.example.com`，让服务器商把实际发信 IP 的 PTR 指回同一个主机名。出站 TCP 25 也要能访问公网。若服务器商限制出站，申请解封或配置 SMTP 中继。[MX 直投和中继](deployment-and-usage.md#原生-mx-直投)

| 类型 | 名称 | 值 |
| --- | --- | --- |
| A | `mail` | 服务器公网 IPv4 |
| MX | `@` | `mail.example.com`，优先级 10 |
| TXT · SPF | `@` | 仅此服务器直投时用 `v=spf1 ip4:实际公网IPv4 ~all`。已有 SPF 就合并授权，根域保留一条 |
| TXT · DKIM | 后台生成 | 复制后台给出的公钥值，发布后回后台启用 |
| TXT · DMARC | `_dmarc` | 新域可先用 `v=DMARC1; p=none`，已有策略则保留 |

Docker 修改配置后执行 `docker compose up -d --no-deps --no-build --wait harbor`。单二进制执行 `sudo systemctl restart harbormail.service`。网页 HTTPS 和 SMTP TCP 25 分别检查。使用桌面邮件客户端时，再按需配置 465 或 587 发信、993 收信。[邮件客户端设置](deployment-and-usage.md#接入邮件客户端)

## 上线验收与日常命令

从外部邮箱发到 Harbor Mail，确认能收到。再从 Harbor Mail 发到自己的 Gmail，检查发件箱结果和 Gmail 的收件箱或垃圾邮件。Gmail 的「显示原始邮件」应能看到 SPF、DKIM、DMARC 的结果。远端 SMTP 返回 `250` 表示已接收，邮件仍可能被分类为垃圾邮件。

| 操作 | Docker | 单二进制 |
| --- | --- | --- |
| 查状态 | `docker compose ps` | `sudo systemctl status harbormail.service --no-pager` |
| 看日志 | `docker compose logs --tail=80 harbor` | `sudo journalctl -u harbormail.service -n 80 --no-pager` |
| 重启 | `docker compose restart harbor` | `sudo systemctl restart harbormail.service` |
| 停止 | `docker compose stop harbor` | `sudo systemctl stop harbormail.service` |

## 升级已有实例

在「管理后台 → 概览 → 关于 Harbor Mail」点击「检查更新」。它会查询 GitHub 的最新稳定版。先阅读发布说明，再备份数据库、配置和 DKIM 密钥。保留原 Compose 项目名、数据卷和凭据。[备份与恢复](../ops/README.md#备份)

**Docker** 沿用原 Compose 文件、项目名和数据挂载。备份后保存旧镜像，再只更新 Harbor Mail。查看 [精简版](deploy/docker-minimal-upgrade.md)、[标准版](deploy/docker-standard-upgrade.md)或[全量版](deploy/docker-full-upgrade.md)的升级步骤。

**Linux 二进制** 下载并校验本机架构的文件，备份数据后替换二进制。查看 [精简版](deploy/binary-minimal-upgrade.md)、[标准版](deploy/binary-standard-upgrade.md)或[全量版](deploy/binary-full-upgrade.md)的升级步骤。

升级后检查服务状态、`/healthz` 和后台显示的版本，再验证一封邮件的收发。
