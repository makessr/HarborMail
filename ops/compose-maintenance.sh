#!/usr/bin/env bash
# Local maintenance for an existing Harbor Mail full Compose deployment.
set -Eeuo pipefail
umask 077

compose_dir=${HARBOR_MAINTENANCE_COMPOSE_DIR:-/opt/harbor-mail}
backup_dir=${HARBOR_MAINTENANCE_BACKUP_DIR:-/var/backups/harbor-mail}
public_url=${HARBOR_MAINTENANCE_PUBLIC_URL:-}
mail_tls=${HARBOR_MAINTENANCE_MAIL_TLS:-}
mode=${1:-}
case $mode in check|backup) ;; *) printf 'Usage: %s check|backup\n' "$0" >&2; exit 2 ;; esac
[[ $backup_dir == /* && $backup_dir != / && -d $backup_dir ]] || exit 2
cd -- "$compose_dir"

mysql_admin() {
  docker compose exec -T mysql sh -eu -c '
    if [ -n "${MYSQL_ROOT_PASSWORD_FILE:-}" ]; then MYSQL_ROOT_PASSWORD=$(cat "$MYSQL_ROOT_PASSWORD_FILE"); fi
    export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"
    exec mysql -uroot --batch --skip-column-names --database="$MYSQL_DATABASE" "$@"
  ' sh "$@"
}

mysql_restore() {
  mysql_admin --init-command='SET SESSION sql_log_bin=0' "$@"
}

check_filesystem() {
  local path=$1 data inode
  data=$(df -P -- "$path" | awk 'NR==2 {sub(/%$/, "", $5); print $5}')
  inode=$(df -Pi -- "$path" | awk 'NR==2 {sub(/%$/, "", $5); print $5}')
  if [[ ! $data =~ ^[0-9]+$ || ! $inode =~ ^[0-9]+$ ]]; then
    printf 'Could not measure filesystem for %s\n' "$path" >&2
    return 1
  fi
  if (( data >= 80 || inode >= 90 )); then
    printf 'Filesystem above threshold %s used=%s%% inode=%s%%\n' "$path" "$data" "$inode" >&2
    return 1
  fi
  printf 'Filesystem %s used=%s%% inode=%s%%\n' "$path" "$data" "$inode"
}

check_certificate() {
  local host=$1 port=$2
  shift 2
  timeout 10 openssl s_client -connect "$host:$port" -servername "$host" \
    -verify_return_error -verify_hostname "$host" "$@" </dev/null 2>/dev/null |
    openssl x509 -noout -checkend 1814400 >/dev/null
}

check_services() {
  local status=0 service id health mount kind oldest ready uncertain scan_errors public_host endpoint
  local -a endpoints=()
  for service in harbor mysql rspamd clamav; do
    id=$(docker compose ps -q "$service")
    if [[ -z $id ]]; then
      printf '%s is missing or stopped\n' "$service" >&2
      status=1
      continue
    fi
    health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$id")
    if [[ $health != healthy && $health != running ]]; then
      printf '%s status=%s\n' "$service" "$health" >&2
      status=1
    fi
    case $service in harbor) kind=/var/lib/harbormail ;; mysql) kind=/var/lib/mysql ;; *) continue ;; esac
    mount=$(docker inspect --format '{{range .Mounts}}{{println .Destination .Source}}{{end}}' "$id" |
      awk -v target="$kind" '$1==target {sub(/^[^ ]+ /, ""); print; exit}')
    if [[ -z $mount ]]; then
      printf 'Could not find data mount for %s\n' "$service" >&2
      status=1
    else
      check_filesystem "$mount" || status=1
    fi
  done
  check_filesystem "$backup_dir" || status=1
  if read -r oldest ready uncertain < <(mysql_admin -e "
      SELECT COALESCE(MAX(CASE WHEN state IN ('queued','scheduled','undo') AND due<=UNIX_TIMESTAMP()
        THEN UNIX_TIMESTAMP()-due WHEN state IN ('preparing','sending') AND lease>0 AND lease<=UNIX_TIMESTAMP()
        THEN UNIX_TIMESTAMP()-lease END),0),
        COALESCE(SUM(state IN ('queued','scheduled','undo') AND due<=UNIX_TIMESTAMP()),0),
        COALESCE(SUM(state='uncertain'),0) FROM outbox"); then
    printf 'Queue oldest_wait=%ss ready=%s uncertain=%s\n' "$oldest" "$ready" "$uncertain"
    if (( oldest > 300 || ready > 200 || uncertain > 0 )); then status=1; fi
  else
    printf 'Could not read the outbox\n' >&2
    status=1
  fi
  if scan_errors=$(docker compose logs --since 15m --no-color harbor 2>&1 |
    awk '/Inbound scanner unavailable or incomplete/ {count++} END {print count+0}'); then
    printf 'Scanner errors during last 15 minutes=%s\n' "$scan_errors"
    if (( scan_errors > 0 )); then status=1; fi
  else
    printf 'Could not read scanner error counts\n' >&2
    status=1
  fi
  if [[ $public_url =~ ^https://([A-Za-z0-9.-]+)$ ]]; then
    public_host=${BASH_REMATCH[1]}
    if ! curl -fsS --max-time 10 "$public_url/healthz" >/dev/null ||
      ! check_certificate "$public_host" 443; then
      printf 'Public HTTPS health or 21-day certificate check failed\n' >&2
      status=1
    fi
    IFS=, read -ra endpoints <<< "$mail_tls"
    for endpoint in "${endpoints[@]}"; do
      case $endpoint in
        25|587) check_certificate "$public_host" "$endpoint" -starttls smtp && continue ;;
        465|993|995) check_certificate "$public_host" "$endpoint" && continue ;;
      esac
      printf 'Mail TLS certificate or handshake check failed on port %s\n' "$endpoint" >&2
      status=1
    done
  else
    printf 'Set HARBOR_MAINTENANCE_PUBLIC_URL to https://mail.example.com\n' >&2
    status=1
  fi
  if [[ ! -f $backup_dir/compose-last-verified || -n $(find "$backup_dir/compose-last-verified" -mmin +1560 -print) ]]; then
    printf 'No verified local backup in the last 26 hours\n' >&2
    status=1
  fi
  return "$status"
}

backup_and_restore() (
  local target schema stamp db_bytes backup_free mysql_free mysql_id mysql_mount
  check_filesystem "$backup_dir"
  exec 9>"$backup_dir/.compose-backup.lock"
  flock -n 9
  mysql_id=$(docker compose ps -q mysql)
  mysql_mount=$(docker inspect --format '{{range .Mounts}}{{println .Destination .Source}}{{end}}' "$mysql_id" |
    awk '$1=="/var/lib/mysql" {sub(/^[^ ]+ /, ""); print; exit}')
  [[ -n $mysql_mount ]]
  check_filesystem "$mysql_mount"
  db_bytes=$(mysql_admin -e 'SELECT COALESCE(SUM(DATA_LENGTH+INDEX_LENGTH),0) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE()')
  backup_free=$(df -PB1 -- "$backup_dir" | awk 'NR==2 {print $4}')
  mysql_free=$(df -PB1 -- "$mysql_mount" | awk 'NR==2 {print $4}')
  if [[ ! $db_bytes =~ ^[0-9]+$ || ! $backup_free =~ ^[0-9]+$ || ! $mysql_free =~ ^[0-9]+$ ]] ||
    (( db_bytes > backup_free / 4 || db_bytes > mysql_free / 4 )); then
    printf 'Insufficient space for a dump and isolated restore\n' >&2
    return 1
  fi
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  target=$(mktemp -d "$backup_dir/compose-$stamp.XXXXXXXX")
  schema="harbor_verify_compose_${stamp//[TZ]/}_$$"
  [[ $schema =~ ^harbor_verify_compose_[0-9]+_[0-9]+$ ]] || exit 1
  docker compose exec -T mysql sh -eu -c '
    if [ -n "${MYSQL_ROOT_PASSWORD_FILE:-}" ]; then MYSQL_ROOT_PASSWORD=$(cat "$MYSQL_ROOT_PASSWORD_FILE"); fi
    export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"
    exec mysqldump -uroot --single-transaction --quick --skip-triggers --no-tablespaces --set-gtid-purged=OFF --hex-blob --max-allowed-packet=64M "$MYSQL_DATABASE"
  ' | gzip -1 > "$target/harbor.sql.gz.part"
  test -s "$target/harbor.sql.gz.part"
  gzip -t "$target/harbor.sql.gz.part"
  mv -- "$target/harbor.sql.gz.part" "$target/harbor.sql.gz"
  (cd "$target"; sha256sum harbor.sql.gz > SHA256SUMS; sha256sum -c SHA256SUMS)
  mysql_restore -e "CREATE DATABASE $schema CHARACTER SET utf8mb4"
  # The trap only removes the disposable database created by this process.
  trap 'mysql_restore -e "DROP DATABASE $schema" >/dev/null' EXIT
  trap 'exit 143' TERM
  trap 'exit 130' INT
  gzip -dc "$target/harbor.sql.gz" | mysql_restore --database="$schema" --max-allowed-packet=64M
  mysql_admin --database="$schema" <<'SQL' > "$target/restore-verification.txt"
SELECT 'letters',COUNT(*),COALESCE(SUM(LENGTH(raw)),0) FROM letters;
SELECT 'outbox',COUNT(*),COALESCE(SUM(LENGTH(raw)),0) FROM outbox;
SELECT 'managed_accounts',COUNT(*) FROM managed_accounts;
SELECT 'dkim_keys',COUNT(*) FROM domain_dkim_keys;
SELECT 'imap_uids',COUNT(*) FROM imap_uids;
SELECT 'quota_rows',COUNT(*) FROM mailbox_quota;
SQL
  mysql_restore -e "DROP DATABASE $schema"
  trap - EXIT
  date -u +%FT%TZ > "$target/verified-at"
  stamp=$(mktemp "$backup_dir/.compose-last-verified.XXXXXXXX")
  cat "$target/verified-at" > "$stamp"
  mv -- "$stamp" "$backup_dir/compose-last-verified"
  printf 'Verified backup: %s\n' "$target"
  # Rotate only this job's verified artifacts. Pre-upgrade and failed backups are left intact.
  find "$backup_dir" -mindepth 1 -maxdepth 1 -type d -name 'compose-????????T??????Z.????????' -mtime +14 -print0 |
    while IFS= read -r -d '' old; do
      if [[ -f $old/verified-at ]]; then
        rm -- "$old/harbor.sql.gz" "$old/SHA256SUMS" "$old/restore-verification.txt" "$old/verified-at"
        rmdir -- "$old"
      fi
    done
)

case $mode in check) check_services ;; backup) backup_and_restore ;; esac
