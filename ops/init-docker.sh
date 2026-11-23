#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

usage() {
  printf '用法：bash ops/init-docker.sh [minimal|standard|full]\n'
  printf '  minimal  Harbor Mail + SQLite\n'
  printf '  standard Harbor Mail + MySQL + Redis（默认）\n'
  printf '  full     标准版 + Rspamd + ClamAV\n'
}

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  usage
  exit 0
fi
if (( $# > 1 )); then
  usage >&2
  exit 2
fi
mode=${1:-standard}
case "$mode" in
  minimal) compose_files=compose.minimal.yaml ;;
  standard) compose_files=compose.standard.yaml ;;
  full) compose_files=compose.full.yaml ;;
  *) usage >&2; exit 2 ;;
esac

if (( EUID != 0 )); then
  printf '请在 Linux root 终端运行，用于设置凭据文件所属组。\n' >&2
  exit 1
fi
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.."
docker compose version >/dev/null
if [[ -L secrets || ( -e secrets && ! -d secrets ) ]]; then
  printf 'secrets 必须为本地目录。\n' >&2
  exit 1
fi

files=(.env secrets/accounts.json)
if [[ $mode != minimal ]]; then
  files+=(secrets/mysql-password secrets/mysql-root-password secrets/mysql-dsn)
fi
for file in "${files[@]}"; do
  if [[ -e $file || -L $file ]]; then
    printf '已有部署文件 %s，请保留现有配置。\n' "$file" >&2
    exit 1
  fi
done

admin_email=${HARBOR_INITIAL_ADMIN_EMAIL:-}
if [[ -z $admin_email && -t 0 ]]; then
  read -rp '输入管理员邮箱（留空使用演示邮箱 hello@harbor.test）: ' admin_email
fi
admin_email=${admin_email:-hello@harbor.test}
admin_email=${admin_email,,}
if [[ ${#admin_email} -gt 254 || ! $admin_email =~ ^[a-z0-9][a-z0-9._%+-]*@([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]]; then
  printf '请输入有效的管理员邮箱。\n' >&2
  exit 1
fi
mail_host=mail.${admin_email#*@}
if [[ ${admin_email#*@} == harbor.test ]]; then
  mail_host=harbor.test
fi

registry=ghcr.io/makessr/harbormail
image=${HARBOR_IMAGE:-}
if [[ -z $image ]]; then
  version=$(git describe --tags --exact-match HEAD 2>/dev/null) || {
    printf '请检出正式发行标签，或显式设置 HARBOR_IMAGE。\n' >&2
    exit 1
  }
  [[ $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
    printf '当前标签不是正式发行版本。\n' >&2
    exit 1
  }
  revision=$(git rev-parse --verify HEAD)
  image="$registry:sha-${revision:0:7}"
fi
[[ $image =~ ^[a-zA-Z0-9][a-zA-Z0-9._/:@-]*$ ]] || {
  printf 'HARBOR_IMAGE 格式无效。\n' >&2
  exit 1
}
docker pull "$image"
if [[ $image == "$registry":* ]]; then
  digests=$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$image")
  while IFS= read -r digest; do
    if [[ $digest == "$registry"@sha256:* ]]; then
      image=$digest
      break
    fi
  done <<< "$digests"
  [[ $image =~ ^ghcr\.io/makessr/harbormail@sha256:[a-f0-9]{64}$ ]] || {
    printf '无法确认官方镜像的摘要。\n' >&2
    exit 1
  }
fi
if ! read -rsp '输入邮箱密码（12–72 字节）: ' mailbox_password; then
  printf '\n未读取到邮箱密码。\n' >&2
  exit 1
fi
printf '\n'
hash=$(printf '%s\n' "$mailbox_password" | docker run --rm -i --network none "$image" hash-password)
unset mailbox_password

if [[ $mode != minimal ]]; then
  mysql_password=$(openssl rand -hex 24)
  mysql_root_password=$(openssl rand -hex 24)
fi

set -o noclobber
install -d -m 0700 secrets
printf '{"%s":"%s"}\n' "$admin_email" "$hash" > secrets/accounts.json
harbor_credentials=(secrets/accounts.json)
if [[ $mode != minimal ]]; then
  printf '%s' "$mysql_password" > secrets/mysql-password
  printf '%s' "$mysql_root_password" > secrets/mysql-root-password
  printf 'harbor:%s@tcp(mysql:3306)/harbor_mail' "$mysql_password" > secrets/mysql-dsn
  harbor_credentials+=(secrets/mysql-dsn)
fi
chgrp 10001 "${harbor_credentials[@]}"
chmod 0640 "${harbor_credentials[@]}"
printf 'COMPOSE_FILE=%s\nHARBOR_ADMINS=%s\nHARBOR_DOMAIN=%s\nHARBOR_PUBLIC_DEFAULTS=true\nHARBOR_IMAGE=%s\n' "$compose_files" "$admin_email" "$mail_host" "$image" > .env
printf '已生成部署配置。启动后使用 %s 登录。\n' "$admin_email"
