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

image=ghcr.io/makessr/harbormail:latest
docker pull "$image"
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
printf 'COMPOSE_FILE=%s\nHARBOR_ADMINS=%s\nHARBOR_DOMAIN=%s\n' "$compose_files" "$admin_email" "$mail_host" > .env
printf '已生成部署配置。启动后使用 %s 登录。\n' "$admin_email"
