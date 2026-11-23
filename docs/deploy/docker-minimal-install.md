# Docker 精简版部署

适合试用、个人邮箱和轻量单机。邮件存入内置 SQLite。 [所有方案](README.md) · [本方案升级](docker-minimal-upgrade.md)

## 1. 启动

在 Linux 服务器准备 Git 和 Docker Compose。 在**新目录**运行。初始化时输入完整管理员邮箱和 12–72 字节的密码。

~~~bash
git clone --branch v0.2.23 --depth 1 https://github.com/makessr/HarborMail.git &&
  cd HarborMail &&
  sudo bash ops/init-docker.sh minimal &&
  sudo docker compose config --quiet &&
  sudo docker compose up -d --no-build --wait --wait-timeout 900
~~~

初始化会为 `compose.minimal.yaml` 写好 `.env` 和 `secrets/`。默认使用 Docker 命名卷，镜像取 GHCR 的 `latest`。

## 2. 验证

~~~bash
sudo docker compose ps
curl -fsS http://127.0.0.1:8080/healthz
~~~

应看到 `harbor` 健康。 健康接口应返回 `ok`。

[打开后台并试发](common.md#打开后台并试发) · [接入公网](../quick-start.md#接入公网)
