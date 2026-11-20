#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

[[ ${RUNNER_ENVIRONMENT:-} == github-hosted ]] || exit 1
: "${HARBOR_TEST_BACKUP_MYSQL_PORT:?}"
: "${HARBOR_TEST_BACKUP_MYSQL_PASSWORD:?}"
: "${HARBOR_TEST_BACKUP_MYSQL_CONTAINER:?}"
: "${MYSQL_ROOT_PASSWORD:?}"

config=/etc/harbor
state=/var/lib/harbor-backup
program=/opt/harbor-mail/ops/backup.sh
unit=/etc/systemd/system/harbor-backup.service
timer=/etc/systemd/system/harbor-backup.timer
for path in "$config" "$state" /opt/harbor-mail "$unit" "$unit.d" "$timer" "$timer.d"; do
  if sudo test -e "$path" || sudo test -L "$path"; then
    printf 'Backup test path already exists: %s\n' "$path" >&2
    exit 1
  fi
done
if getent passwd harbor-backup >/dev/null; then
  printf 'Backup test user already exists.\n' >&2
  exit 1
fi

work=$(mktemp -d)
restore_work=$(sudo mktemp -d /run/harbor-restore.XXXXXXXX)
cleanup() {
  local status=$?
  trap - EXIT
  if (( status != 0 )); then sudo journalctl -u harbor-backup.service --no-pager -n 40 >&2 || true; fi
  sudo systemctl stop harbor-backup.timer harbor-backup.service || true
  sudo systemctl clean --what=state harbor-backup.service || true
  sudo rm -rf -- "$restore_work"
  rm -rf -- "$work"
  exit "$status"
}
trap cleanup EXIT

sudo useradd --system --user-group --home-dir "$state" --shell "$(command -v nologin)" harbor-backup
sudo install -d -m 0700 "$config"
sudo install -d -m 0755 "$(dirname "$program")" "$unit.d" "$timer.d"
openssl rand -hex 32 > "$work/restic-password"
sudo install -m 0600 "$work/restic-password" "$config/"
sudo install -m 0755 ops/backup.sh "$program"
sudo install -m 0644 ops/harbor-backup.service "$unit"
sudo install -m 0644 ops/harbor-backup.timer "$timer"
sudo tee "$timer.d/fixture.conf" >/dev/null <<'TIMER'
[Timer]
OnCalendar=
OnActiveSec=2s
AccuracySec=100ms
RandomizedDelaySec=0
Persistent=false
TIMER

exercise_timer() {
  local driver=$1 actor=$2 subject=$3
  local repository="$state/$driver-repo" instance="ci-$driver"
  local metrics="$state/$driver-metrics" marker snapshot destination previous line attempt
  local -a mysql_credentials=()
  marker="$metrics/harbor-backup.prom"
  destination="$restore_work/$driver"
  sudo install -d -o "$actor" -g "$actor" -m 0700 "$state" "$metrics"
  sudo systemd-run --quiet --wait --pipe --collect -p "User=$actor" \
    -p "LoadCredential=restic-password:$config/restic-password" \
    /bin/bash -c 'RESTIC_PASSWORD_FILE="$CREDENTIALS_DIRECTORY/restic-password" exec restic -r "$1" init' restic "$repository"
  {
    printf 'HARBOR_BACKUP_INSTANCE=%s\nHARBOR_BACKUP_DRIVER=%s\n' "$instance" "$driver"
    printf 'RESTIC_REPOSITORY=%s\nHARBOR_BACKUP_METRIC_DIR=%s\n' "$repository" "$metrics"
    printf 'HARBOR_BACKUP_MYSQL_DB=harbor_test\nHARBOR_BACKUP_SQLITE=/var/lib/harbormail/harbor.db\n'
  } > "$work/backup.env"
  sudo install -m 0600 "$work/backup.env" "$config/backup.env"
  if [[ $driver == mysql ]]; then
    mysql_credentials=(-p "LoadCredential=mysql-backup.cnf:$config/mysql-backup.cnf")
  fi
  {
    printf '[Service]\nUser=%s\n' "$actor"
    printf 'LoadCredential=restic-password:%s/restic-password\n' "$config"
    printf '%s\n' 'Environment=RESTIC_PASSWORD_FILE=%d/restic-password'
    if [[ $driver == mysql ]]; then
      printf 'LoadCredential=mysql-backup.cnf:%s/mysql-backup.cnf\n' "$config"
      printf '%s\n' 'Environment=HARBOR_BACKUP_MYSQL_CNF=%d/mysql-backup.cnf'
    fi
  } | sudo tee "$unit.d/fixture.conf" >/dev/null
  sudo systemd-run --quiet --wait --pipe --collect -p "User=$actor" \
    -p "StateDirectory=harbor-backup" -p 'StateDirectoryMode=0700' \
    -p "EnvironmentFile=$config/backup.env" -p "Environment=HARBOR_BACKUP_WORK=$state" \
    -p "LoadCredential=restic-password:$config/restic-password" "${mysql_credentials[@]}" \
    /usr/bin/bash -c 'RESTIC_PASSWORD_FILE="$CREDENTIALS_DIRECTORY/restic-password" HARBOR_BACKUP_MYSQL_CNF="$CREDENTIALS_DIRECTORY/mysql-backup.cnf" exec bash "$1" check' bash "$program"
  ! sudo test -e "$marker"
  sudo restic -r "$repository" --password-file "$config/restic-password" --no-lock snapshots --json |
    jq -e 'length == 0' >/dev/null
  sudo systemd-analyze verify "$unit" "$timer"
  sudo systemctl daemon-reload
  sudo systemctl start harbor-backup.timer
  for attempt in {1..120}; do
    if sudo test -s "$marker" && [[ $(systemctl show -p ActiveState --value harbor-backup.service) == inactive ]]; then
      break
    fi
    if systemctl is-failed --quiet harbor-backup.service; then return 1; fi
    sleep .25
  done
  sudo systemctl stop harbor-backup.timer
  [[ $(systemctl show -p Result --value harbor-backup.service) == success ]]
  [[ $(sudo stat -c '%a:%U' "$state") == "700:$actor" ]]
  [[ $(sudo stat -c '%a:%U' "$marker") == "644:$actor" ]]
  previous=$(sudo cat "$marker")
  if sudo flock -n "$state/harbor-mail-$instance.lock" systemctl start harbor-backup.service; then
    printf 'Backup service accepted a concurrent run.\n' >&2
    return 1
  fi
  line=$(sudo journalctl -u harbor-backup.service --no-pager -n 20)
  [[ $line == *'Another backup or verification is running'* ]]
  [[ $(sudo cat "$marker") == "$previous" ]]
  sudo systemctl reset-failed harbor-backup.service

  snapshot=$(sudo restic -r "$repository" --password-file "$config/restic-password" snapshots --json |
    jq -er 'select(length == 1) | .[0].id')
  sudo env HARBOR_BACKUP_WORK="$restore_work" HARBOR_BACKUP_INSTANCE="$instance" \
    RESTIC_REPOSITORY="$repository" RESTIC_PASSWORD_FILE="$config/restic-password" \
    bash "$program" export "$snapshot" "$destination"
  sudo bash -c 'cd "$1"; sha256sum -c SHA256SUMS' sha256sum "$destination"
  if [[ $driver == sqlite ]]; then
    [[ $(sudo sqlite3 -readonly "$destination/harbor.db" "SELECT COUNT(*) FROM letters WHERE subject='$subject'") == 1 ]]
    [[ $(sudo sqlite3 -readonly "$destination/harbor.db" "SELECT hex(sealed_key) FROM domain_dkim_keys WHERE domain='backup-key.test'") == 0123456789ABCDEF ]]
  else
    sudo cat "$destination/harbor.sql" | docker exec -i -e MYSQL_PWD="$MYSQL_ROOT_PASSWORD" \
      "$HARBOR_TEST_BACKUP_MYSQL_CONTAINER" mysql --user=root harbor_verify_backup
    [[ $(docker exec -e MYSQL_PWD="$MYSQL_ROOT_PASSWORD" "$HARBOR_TEST_BACKUP_MYSQL_CONTAINER" \
      mysql --user=root --batch --skip-column-names harbor_verify_backup \
      -e "SELECT COUNT(*) FROM letters WHERE subject='$subject'") == 1 ]]
    [[ $(docker exec -e MYSQL_PWD="$MYSQL_ROOT_PASSWORD" "$HARBOR_TEST_BACKUP_MYSQL_CONTAINER" \
      mysql --user=root --batch --skip-column-names harbor_verify_backup \
      -e "SELECT hex(sealed_key) FROM domain_dkim_keys WHERE domain='backup-key.test'") == 0123456789ABCDEF ]]
  fi
  printf '%s backup timer passed scheduling, locking, permissions, and recovery checks.\n' "$driver"
}

printf '[client]\nuser=harbor_backup\npassword=%s\nhost=127.0.0.1\nport=%s\nprotocol=tcp\n' \
  "$HARBOR_TEST_BACKUP_MYSQL_PASSWORD" "$HARBOR_TEST_BACKUP_MYSQL_PORT" > "$work/mysql-backup.cnf"
sudo install -m 0600 "$work/mysql-backup.cnf" "$config/"
key_fixture="INSERT INTO domain_dkim_keys(domain,slot,selector,public_key,sealed_key)
  VALUES('backup-key.test','active','fixture','fixture-public',X'0123456789ABCDEF');"
sudo -u harbormail sqlite3 -batch -bail /var/lib/harbormail/harbor.db "$key_fixture"
docker exec -e MYSQL_PWD="$MYSQL_ROOT_PASSWORD" "$HARBOR_TEST_BACKUP_MYSQL_CONTAINER" \
  mysql --user=root harbor_test -e "$key_fixture"
exercise_timer sqlite harbormail "SQLite native service"
exercise_timer mysql harbor-backup "MySQL native service"

# A table-scoped backup account can get a successful but incomplete mysqldump.
mysql_root=(docker exec -i -e MYSQL_PWD="$MYSQL_ROOT_PASSWORD" "$HARBOR_TEST_BACKUP_MYSQL_CONTAINER"
  mysql --user=root --batch --skip-column-names)
limited_password=$(openssl rand -hex 24)
printf '::add-mask::%s\n' "$limited_password"
"${mysql_root[@]}" -e "CREATE USER 'harbor_without_dkim'@'%' IDENTIFIED BY '$limited_password'"
"${mysql_root[@]}" -e "SELECT CONCAT('GRANT SELECT, SHOW VIEW ON harbor_test.',table_name,
  ' TO ',QUOTE('harbor_without_dkim'),'@',QUOTE('%'),';') FROM information_schema.tables
  WHERE table_schema='harbor_test' AND table_name REGEXP '^[a-z_]+$'
  AND table_name NOT IN ('domain_dkim_keys','outbox_dkim_signatures');" | "${mysql_root[@]}"
printf '[client]\nuser=harbor_without_dkim\npassword=%s\nhost=127.0.0.1\nport=%s\nprotocol=tcp\n' \
  "$limited_password" "$HARBOR_TEST_BACKUP_MYSQL_PORT" > "$work/mysql-limited.cnf"
sudo install -m 0600 "$work/mysql-limited.cnf" "$config/"
for action in check backup; do
  if output=$(sudo systemd-run --quiet --wait --pipe --collect -p User=harbor-backup \
    -p "Environment=HARBOR_BACKUP_WORK=$state" -p "EnvironmentFile=$config/backup.env" \
    -p "LoadCredential=restic-password:$config/restic-password" \
    -p "LoadCredential=mysql-backup.cnf:$config/mysql-limited.cnf" \
    /usr/bin/bash -c 'RESTIC_PASSWORD_FILE="$CREDENTIALS_DIRECTORY/restic-password" HARBOR_BACKUP_MYSQL_CNF="$CREDENTIALS_DIRECTORY/mysql-backup.cnf" exec bash "$1" "$2"' \
    bash "$program" "$action" 2>&1); then
    printf 'MySQL %s accepted a dump without DKIM tables.\n' "$action" >&2
    exit 1
  fi
  [[ $output == *'missing required Harbor Mail tables: domain_dkim_keys outbox_dkim_signatures'* ]]
done
sudo restic -r "$state/mysql-repo" --password-file "$config/restic-password" --no-lock snapshots --json |
  jq -e 'length == 1' >/dev/null
printf 'MySQL check and backup rejected a restricted account missing both DKIM tables.\n'
