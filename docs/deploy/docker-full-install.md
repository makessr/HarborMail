# Docker 全量版部署

适合需要来信反垃圾与病毒隔离的站点。Compose 会启动标准版组件、Rspamd、ClamAV 和扫描用 Redis。 [所有方案](README.md) · [本方案升级](docker-full-upgrade.md)

## 1. 启动

在 Linux 服务器准备 Git、OpenSSL 和 Docker Compose 2.23.1 或更新版本。首次加载病毒库可能需要几分钟。 在**新目录**运行。初始化时输入完整管理员邮箱和 12–72 字节的密码。

~~~bash
git clone --branch v0.2.23 --depth 1 https://github.com/makessr/HarborMail.git &&
  cd HarborMail &&
  sudo bash ops/init-docker.sh full &&
  sudo docker compose config --quiet &&
  sudo docker compose up -d --no-build --wait --wait-timeout 900
~~~

初始化会为 `compose.full.yaml` 写好 `.env` 和 `secrets/`。默认使用 Docker 命名卷，镜像取 GHCR 的 `latest`。

## 2. 验证

~~~bash
sudo docker compose ps
curl -fsS http://127.0.0.1:8080/healthz
~~~

应看到 `harbor`、`mysql`、`redis`、`rspamd`、`clamav`、`rspamd-redis` 正在运行。ClamAV 应显示健康。 健康接口应返回 `ok`。

扫描未就绪时，先执行 `sudo docker compose logs --tail=80 clamav rspamd`，确认病毒库下载与加载状态。登录后台后在「安全 → 公网部署安全检查」运行来信扫描检查。默认扫描故障返回 SMTP 451，命中病毒的来信进入「隔离邮件」。

[打开后台并试发](common.md#打开后台并试发) · [接入公网](../quick-start.md#接入公网)
