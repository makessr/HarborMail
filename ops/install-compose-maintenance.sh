#!/usr/bin/env bash
# Install and enable the Harbor Mail Compose backup and health-check timers.
#
# Run with sudo on the Compose host. The script is idempotent: it never
# overwrites an existing environment file without --force, and it reuses the
# values already recorded there.
#
# Environment:
#   HARBOR_INSTALL_ROOT   Optional staging root prefix for files (default "/").
#                         A non-empty value skips the root check, disables the
#                         systemd verification and cannot enable timers.
#   SYSTEMCTL, DOCKER      Command overrides for the systemctl and docker binaries.
set -Eeuo pipefail
umask 077

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
systemctl_bin=${SYSTEMCTL:-systemctl}
docker_bin=${DOCKER:-docker}
stage=${HARBOR_INSTALL_ROOT:-}
stage=${stage%/}

compose_dir=/opt/harbor-mail
backup_dir=/var/backups/harbor-mail
public_url=
mail_tls=
dry_run=0
force=0
enable=1
smoke=1
run_backup=0

usage() {
  cat <<'USAGE'
用法: sudo bash ops/install-compose-maintenance.sh --public-url https://mail.example.com [选项]

安装并启用 Harbor Mail 全量 Compose 的同机备份与巡检定时任务。

选项:
  --public-url URL   站点对外 HTTPS 地址，用于证书与 /healthz 巡检（必填）
  --compose-dir DIR  Compose 部署目录（默认 /opt/harbor-mail）
  --backup-dir DIR   同机备份目录，由 root 独占（默认 /var/backups/harbor-mail）
  --mail-tls PORTS   对外提供 TLS 的邮件端口，逗号分隔，例如 25,587,993
  --run-backup       安装后完整运行一次备份，而不只运行巡检
  --no-enable        只安装并试运行，不启用定时器
  --skip-smoke       跳过安装后的试运行
  --force            覆盖已存在的 /etc/harbor/compose-maintenance.env
  --dry-run          只打印将要执行的操作
  -h, --help         显示本帮助

示例:
  sudo bash ops/install-compose-maintenance.sh \
    --public-url https://mail.example.com --mail-tls 25,587,993
USAGE
}

die() { printf '错误: %s\n' "$1" >&2; exit 1; }
note() { printf '%s\n' "$*"; }
plan() { printf '+ %s\n' "$*"; }

while (($#)); do
  case $1 in
    --public-url) public_url=${2-}; shift 2 ;;
    --public-url=*) public_url=${1#*=}; shift ;;
    --compose-dir) compose_dir=${2-}; shift 2 ;;
    --compose-dir=*) compose_dir=${1#*=}; shift ;;
    --backup-dir) backup_dir=${2-}; shift 2 ;;
    --backup-dir=*) backup_dir=${1#*=}; shift ;;
    --mail-tls) mail_tls=${2-}; shift 2 ;;
    --mail-tls=*) mail_tls=${1#*=}; shift ;;
    --run-backup) run_backup=1; shift ;;
    --no-enable) enable=0; shift ;;
    --skip-smoke) smoke=0; shift ;;
    --force) force=1; shift ;;
    --dry-run) dry_run=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "未知参数: $1" ;;
  esac
done

[[ -n $public_url ]] || { usage >&2; die '必须提供 --public-url'; }
public_url=${public_url%/}

for value in "$compose_dir" "$backup_dir" "$public_url" "$mail_tls"; do
  [[ $value == *[[:space:]]* ]] && die "路径和地址不能包含空白字符: $value"
done

[[ $compose_dir == /* ]] || die "--compose-dir 必须是绝对路径: $compose_dir"
[[ $backup_dir == /* ]] || die "--backup-dir 必须是绝对路径: $backup_dir"
[[ $public_url =~ ^https://[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?(/[^[:space:]]*)?$ ]] ||
  die "--public-url 必须是 https://host[:port] 形式: $public_url"
if [[ -n $mail_tls ]]; then
  IFS=, read -r -a ports <<<"$mail_tls"
  for port in "${ports[@]}"; do
    [[ $port =~ ^[0-9]{1,5}$ ]] && ((10#$port >= 1 && 10#$port <= 65535)) ||
      die "--mail-tls 端口无效: $port"
  done
fi

if [[ -z $stage ]]; then
  [[ $(id -u) == 0 ]] || die '需要 root 权限，请使用 sudo 运行'
  for cmd in "$systemctl_bin" "$docker_bin" install sed; do
    command -v "$cmd" >/dev/null || die "缺少命令: $cmd"
  done
fi
if ((enable)) && [[ -n $stage ]]; then
  die 'HARBOR_INSTALL_ROOT 只用于预置文件，不能启用定时器；请去掉该变量或加上 --no-enable'
fi

compose_file=
for candidate in compose.yaml compose.yml docker-compose.yaml docker-compose.yml; do
  if [[ -f $compose_dir/$candidate ]]; then compose_file=$compose_dir/$candidate; break; fi
done
[[ -n $compose_file ]] || die "未在 $compose_dir 找到 Compose 文件，请用 --compose-dir 指定部署目录"

[[ -f $script_dir/compose-maintenance.sh ]] || die "缺少 $script_dir/compose-maintenance.sh"
for unit in harbor-compose-maintenance@.service harbor-compose-check.timer harbor-compose-backup.timer; do
  [[ -f $script_dir/$unit ]] || die "缺少 $script_dir/$unit"
done

systemd_dir=$stage/etc/systemd/system
config_dir=$stage/etc/harbor
env_file=$config_dir/compose-maintenance.env
installed_script=$compose_dir/ops/compose-maintenance.sh
service_unit=$systemd_dir/harbor-compose-maintenance@.service

if [[ -e $env_file ]] && ((!force)); then
  die "$env_file 已存在；确认覆盖请加 --force，或先手动编辑该文件"
fi

note "Compose 部署目录: $compose_dir（$compose_file）"
note "同机备份目录:   $backup_dir"
note "巡检站点地址:   $public_url"
note "邮件 TLS 端口:  ${mail_tls:-未设置}"
if ((dry_run)); then
  note '--- 试运行（--dry-run），不写入任何文件 ---'
fi

write_file() {
  # write_file MODE DEST；内容从标准输入读入
  local mode=$1 dest=$2 tmp
  if ((dry_run)); then
    plan "写入 $dest（权限 $mode）"
    cat >/dev/null
    return
  fi
  tmp=$(mktemp)
  cat >"$tmp"
  install -m "$mode" -- "$tmp" "$dest"
  rm -f -- "$tmp"
}

ensure_dir() {
  local mode=$1 dir=$2
  mkdir -p -- "$dir"
  chmod "$mode" -- "$dir"
}

if ((dry_run)); then
  plan "创建目录 $config_dir（0700）"
else
  ensure_dir 0700 "$config_dir"
fi

{
  printf '# Harbor Mail Compose 备份与巡检配置，由 ops/install-compose-maintenance.sh 生成。\n'
  printf 'HARBOR_MAINTENANCE_COMPOSE_DIR=%s\n' "$compose_dir"
  printf 'HARBOR_MAINTENANCE_BACKUP_DIR=%s\n' "$backup_dir"
  printf 'HARBOR_MAINTENANCE_PUBLIC_URL=%s\n' "$public_url"
  if [[ -n $mail_tls ]]; then printf 'HARBOR_MAINTENANCE_MAIL_TLS=%s\n' "$mail_tls"; fi
} | write_file 0600 "$env_file"

if [[ -d $backup_dir ]]; then
  if ((!dry_run)); then
    [[ -w $backup_dir ]] || die "备份目录不可写: $backup_dir"
  fi
elif ((dry_run)); then
  plan "创建目录 $backup_dir（0700）"
else
  ensure_dir 0700 "$backup_dir"
fi

if ((dry_run)); then
  plan "安装 $installed_script（0755）"
else
  ensure_dir 0755 "$compose_dir/ops"
  if [[ $(readlink -f -- "$script_dir/compose-maintenance.sh") != $(readlink -f -- "$installed_script") ]]; then
    install -m 0755 -- "$script_dir/compose-maintenance.sh" "$installed_script"
  fi
fi

if ((dry_run)); then
  plan "安装 $service_unit 与两个定时器到 $systemd_dir（0644）"
  plan "生成单元文件，将 /opt/harbor-mail 替换为 $compose_dir"
else
  ensure_dir 0755 "$systemd_dir"
  escaped=${compose_dir//|/\\|}
  sed "s|/opt/harbor-mail|$escaped|g" -- "$script_dir/harbor-compose-maintenance@.service" |
    write_file 0644 "$service_unit"
  for unit in harbor-compose-check.timer harbor-compose-backup.timer; do
    install -m 0644 -- "$script_dir/$unit" "$systemd_dir/$unit"
  done
fi

schedule_note='巡检每五分钟运行一次，备份每天 04:15（Asia/Shanghai）运行并验证回读'

if ((dry_run)); then
  plan "$systemctl_bin daemon-reload"
  ((smoke)) && plan "$systemctl_bin start harbor-compose-maintenance@check.service"
  if ((smoke && run_backup)); then
    plan "$systemctl_bin start harbor-compose-maintenance@backup.service"
  fi
  ((enable)) && plan "$systemctl_bin enable --now harbor-compose-check.timer harbor-compose-backup.timer"
  note "--- 未做任何改动。$schedule_note ---"
  exit 0
fi

"$systemctl_bin" daemon-reload

if command -v systemd-analyze >/dev/null && [[ -z $stage ]]; then
  if ! systemd-analyze verify harbor-compose-maintenance@check.service >&2; then
    die 'systemd-analyze 校验失败，请检查上面的输出后重试'
  fi
fi

if ((smoke)); then
  note '试运行巡检服务…'
  if ! "$systemctl_bin" start harbor-compose-maintenance@check.service; then
    "$systemctl_bin" --no-pager status harbor-compose-maintenance@check.service >&2 || true
    die '巡检服务运行失败；修复后重新运行本脚本'
  fi
  if ((run_backup)); then
    note '试运行备份服务（完整转储并回读，可能需要数分钟）…'
    if ! "$systemctl_bin" start harbor-compose-maintenance@backup.service; then
      "$systemctl_bin" --no-pager status harbor-compose-maintenance@backup.service >&2 || true
      die '备份服务运行失败；修复后重新运行本脚本'
    fi
  fi
fi

if ((enable)); then
  "$systemctl_bin" enable --now harbor-compose-check.timer harbor-compose-backup.timer
fi

note ''
note "已安装 $env_file、$service_unit 与两个定时器。$schedule_note。"
if ((enable)); then
  "$systemctl_bin" list-timers 'harbor-compose-*' --no-pager || true
else
  note '定时器未启用（--no-enable）；确认试运行成功后执行：'
  note "  sudo $systemctl_bin enable --now harbor-compose-check.timer harbor-compose-backup.timer"
fi
if ((!run_backup)); then
  note "确认备份可用： sudo $systemctl_bin start harbor-compose-maintenance@backup.service"
fi
note "查看结果： journalctl -u 'harbor-compose-maintenance@*.service' -n 50 --no-pager"
