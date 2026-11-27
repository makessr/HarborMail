#!/usr/bin/env bash
# Exercise ops/install-compose-maintenance.sh in a staging root with fake
# systemctl and docker binaries. No root, Docker or systemd required.
set -Eeuo pipefail

repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
installer=$repo/ops/install-compose-maintenance.sh
work=$(mktemp -d)
cleanup() {
  local status=$?
  rm -rf -- "$work"
  exit "$status"
}
trap cleanup EXIT

fake_bin=$work/bin
mkdir -p "$fake_bin"
cat >"$fake_bin/systemctl" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${SYSTEMCTL_LOG:?}"
if [[ ${1-} == list-timers ]]; then
  printf 'NEXT  LEFT  LAST  PASSED  UNIT  ACTIVATES\n'
fi
exit 0
FAKE
cat >"$fake_bin/docker" <<'FAKE'
#!/usr/bin/env bash
exit 0
FAKE
chmod 0755 "$fake_bin/systemctl" "$fake_bin/docker"

export PATH=$fake_bin:$PATH
export SYSTEMCTL_LOG=$work/systemctl.log
export HARBOR_INSTALL_ROOT=$work/root

stage=$work/root
compose_dir=$stage/opt/harbor-mail
backup_dir=$stage/var/backups/harbor-mail
env_file=$stage/etc/harbor/compose-maintenance.env
service_unit=$stage/etc/systemd/system/harbor-compose-maintenance@.service
mkdir -p "$compose_dir"
printf 'services: {}\n' >"$compose_dir/compose.yaml"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# Windows 的 MSYS 环境按 ACL 合成权限，chmod 结果无法可靠回读；CI 在 Linux 上
# 运行本脚本，权限断言只在那里生效。
check_modes=1
case $(uname -s 2>/dev/null || printf unknown) in
  MINGW*|MSYS*|CYGWIN*) check_modes=0 ;;
esac
check_mode() {
  local expected=$1 path=$2 actual
  ((check_modes)) || return 0
  [[ -e $path ]] || fail "缺少 $path"
  actual=$(stat -c '%a' "$path")
  [[ $actual == "$expected" ]] || fail "$path 权限应为 $expected，实际 $actual"
}

expect_ok() {
  local output
  if ! output=$(bash "$installer" "$@" 2>&1); then
    printf '%s\n' "$output" >&2
    fail "安装脚本应成功: $*"
  fi
}
expect_fail() {
  local output
  if output=$(bash "$installer" "$@" 2>&1); then
    printf '%s\n' "$output" >&2
    fail "安装脚本应失败: $*"
  fi
}

base_args=(
  --public-url https://mail.example.test
  --compose-dir "$compose_dir"
  --backup-dir "$backup_dir"
  --mail-tls 25,587,993
  --no-enable
)

# --dry-run 不写入任何文件，也不调用 systemctl
expect_ok --dry-run "${base_args[@]}"
[[ -e $env_file ]] && fail '--dry-run 不应创建环境文件'
[[ -e $service_unit ]] && fail '--dry-run 不应安装单元文件'
[[ ! -s $SYSTEMCTL_LOG ]] || fail '--dry-run 不应调用 systemctl'

# 参数与目录校验
expect_fail --public-url http://mail.example.test --compose-dir "$compose_dir" --no-enable
expect_fail --public-url https://mail.example.test --compose-dir "$compose_dir" --no-enable --mail-tls 0
expect_fail --public-url https://mail.example.test --compose-dir "$compose_dir" --no-enable --mail-tls 25,70000
expect_fail --public-url https://mail.example.test --compose-dir "$compose_dir/../nope" --no-enable
expect_fail --public-url https://mail.example.test --compose-dir "$compose_dir" --no-enable \
  --backup-dir 'relative/backup'
expect_fail --public-url https://mail.example.test --compose-dir "$compose_dir" --no-enable --bogus

# 预置根目录下不能启用定时器
expect_fail --public-url https://mail.example.test --compose-dir "$compose_dir" --backup-dir "$backup_dir"

# 正常安装
expect_ok "${base_args[@]}"
[[ -f $env_file ]] || fail '缺少环境文件'
check_mode 600 "$env_file"
check_mode 700 "$stage/etc/harbor"
grep -qx "HARBOR_MAINTENANCE_COMPOSE_DIR=$compose_dir" "$env_file" || fail '环境文件缺少 Compose 目录'
grep -qx "HARBOR_MAINTENANCE_BACKUP_DIR=$backup_dir" "$env_file" || fail '环境文件缺少备份目录'
grep -qx 'HARBOR_MAINTENANCE_PUBLIC_URL=https://mail.example.test' "$env_file" || fail '环境文件缺少站点地址'
grep -qx 'HARBOR_MAINTENANCE_MAIL_TLS=25,587,993' "$env_file" || fail '环境文件缺少邮件端口'
check_mode 700 "$backup_dir"
[[ -x $compose_dir/ops/compose-maintenance.sh ]] || fail '未安装维护脚本'
check_mode 644 "$service_unit"
grep -qx "ExecStart=$compose_dir/ops/compose-maintenance.sh %i" "$service_unit" ||
  fail '服务单元未指向部署目录下的脚本'
grep -qx 'EnvironmentFile=/etc/harbor/compose-maintenance.env' "$service_unit" ||
  fail '服务单元的环境文件路径被改动'
for unit in harbor-compose-check.timer harbor-compose-backup.timer; do
  [[ -f $stage/etc/systemd/system/$unit ]] || fail "缺少 $unit"
  check_mode 644 "$stage/etc/systemd/system/$unit"
done
grep -qx 'daemon-reload' "$SYSTEMCTL_LOG" || fail '未重新加载 systemd'
grep -qx 'start harbor-compose-maintenance@check.service' "$SYSTEMCTL_LOG" || fail '未试运行巡检服务'
if grep -q 'enable --now' "$SYSTEMCTL_LOG"; then fail '--no-enable 时不应启用定时器'; fi

# 默认不覆盖已有配置；--force 才重写，且权限保持 600
expect_fail "${base_args[@]}"
grep -qx "HARBOR_MAINTENANCE_COMPOSE_DIR=$compose_dir" "$env_file" || fail '失败的重跑不应破坏环境文件'
expect_ok --force "${base_args[@]}"
check_mode 600 "$env_file"
check_mode 700 "$backup_dir"

# 自定义部署目录会替换单元里的默认路径；未提供的项不保留旧值
alt=$stage/srv/harbor-mail
mkdir -p "$alt"
printf 'services: {}\n' >"$alt/compose.yaml"
expect_ok --force --public-url https://alt.example.test --compose-dir "$alt" \
  --backup-dir "$backup_dir" --no-enable
grep -qx "ExecStart=$alt/ops/compose-maintenance.sh %i" "$service_unit" ||
  fail '自定义部署目录未写入服务单元'
grep -qx 'HARBOR_MAINTENANCE_PUBLIC_URL=https://alt.example.test' "$env_file" || fail '--force 未更新站点地址'
if grep -q 'HARBOR_MAINTENANCE_MAIL_TLS' "$env_file"; then fail '未提供 --mail-tls 时不应保留旧端口'; fi

printf 'Compose maintenance installer passed staging-root, idempotency, and validation checks.\n'
