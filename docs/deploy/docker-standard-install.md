# Docker 标准版部署

推荐首次长期部署。Compose 会一起启动 MySQL 和 Redis。 [所有方案](README.md) · [本方案升级](docker-standard-upgrade.md)

## 1. 启动

在 Linux 服务器准备 Git、OpenSSL 和 Docker Compose。 在**新目录**运行。初始化时输入完整管理员邮箱和 12–72 字节的密码。

~~~bash
release_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' https://github.com/makessr/HarborMail/releases/latest) &&
version=${release_url##*/} &&
[[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] &&
git clone --branch "$version" --depth 1 https://github.com/makessr/HarborMail.git &&
  cd HarborMail &&
  sudo bash ops/init-docker.sh standard &&
  sudo docker compose config --quiet &&
  sudo docker compose up -d --no-build --wait --wait-timeout 900
~~~

初始化会为 `compose.standard.yaml` 写好 `.env` 和 `secrets/`。默认使用 Docker 命名卷，初始化按检出的发行标签选择同提交镜像，并在 `.env` 记录镜像摘要。

## 2. 验证

~~~bash
sudo docker compose ps
curl -fsS http://127.0.0.1:8080/healthz
~~~

应看到 `harbor`、`mysql`、`redis` 健康。 健康接口应返回 `ok`。

[打开后台并试发](common.md#打开后台并试发) · [接入公网](../quick-start.md#接入公网)
