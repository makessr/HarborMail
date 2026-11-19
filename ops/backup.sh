#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

fail() { printf 'Harbor Mail backup: %s\n' "$*" >&2; exit 1; }

mode=${1:-}
case "$mode" in
  backup|prune|plan) [[ $# == 1 ]] || fail "Usage: backup.sh $mode" ;;
  verify) [[ $# == 2 && $2 =~ ^[0-9a-f]{8,64}$ ]] || fail 'Usage: backup.sh verify SNAPSHOT_ID' ;;
  *) fail 'Usage: backup.sh {backup|plan|prune|verify SNAPSHOT_ID}' ;;
esac

: "${HARBOR_BACKUP_WORK:?Set HARBOR_BACKUP_WORK to a private, persistent directory}"
: "${HARBOR_BACKUP_INSTANCE:?Set HARBOR_BACKUP_INSTANCE to a unique name for this installation}"
: "${RESTIC_REPOSITORY:?Set RESTIC_REPOSITORY to the initialized off-site repository}"
: "${RESTIC_PASSWORD_FILE:?Set RESTIC_PASSWORD_FILE to a private password file}"
[[ $HARBOR_BACKUP_INSTANCE =~ ^[a-zA-Z0-9_-]{1,48}$ ]] || fail 'Invalid backup instance name'
[[ $HARBOR_BACKUP_WORK == /* && -d $HARBOR_BACKUP_WORK && -O $HARBOR_BACKUP_WORK && ! -L $HARBOR_BACKUP_WORK ]] ||
  fail 'Backup work directory must exist, be owned by this user and use an absolute path'
[[ $(stat -c '%a' -- "$HARBOR_BACKUP_WORK") == 700 ]] || fail 'Use mode 0700 for the backup work directory'
[[ $RESTIC_PASSWORD_FILE == /* && -r $RESTIC_PASSWORD_FILE ]] || fail 'Restic password file must be an absolute readable path'
command -v restic >/dev/null || fail 'restic is required'
command -v flock >/dev/null || fail 'flock is required'

tag="harbor-mail-$HARBOR_BACKUP_INSTANCE"
exec 9>"$HARBOR_BACKUP_WORK/$tag.lock"
flock -n 9 || fail 'Another backup or verification is running'

if [[ $mode == plan || $mode == prune ]]; then
  options=(--tag "$tag" --group-by tags --keep-daily 14 --keep-weekly 4)
  if [[ $mode == plan ]]; then options+=(--dry-run); else options+=(--prune); fi
  restic forget "${options[@]}"
  exit
fi

work_root=$(realpath -- "$HARBOR_BACKUP_WORK")
scratch=$(mktemp -d -- "$work_root/harbor.XXXXXXXX")
[[ $(realpath -- "$scratch") == "$work_root"/harbor.* ]] || fail 'Invalid temporary backup directory'
marker_tmp=
cleanup() {
  local result=$?
  trap - EXIT
  if [[ -n $marker_tmp ]]; then rm -f -- "$marker_tmp"; fi
  rm -rf -- "$scratch"
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

verify_sqlite() {
  command -v sqlite3 >/dev/null || fail 'sqlite3 is required'
  [[ $(sqlite3 -batch -bail -readonly "$1" 'PRAGMA integrity_check;') == ok ]] ||
    fail 'SQLite integrity check failed'
  [[ $(sqlite3 -batch -bail -readonly "$1" "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('letters','outbox');") == 2 ]] ||
    fail 'The snapshot does not contain the Harbor Mail tables'
}

read_back() {
  local snapshot=$1 destination=$2
  restic restore "$snapshot" --target "$destination" --verify >&2
  (
    cd "$destination"
    [[ -f SHA256SUMS ]] || fail 'Snapshot does not contain a checksum manifest'
    sha256sum --check --strict SHA256SUMS
  )
}

if [[ $mode == verify ]]; then
  read_back "$2" "$scratch/restored"
  if [[ -f $scratch/restored/harbor.db ]]; then
    verify_sqlite "$scratch/restored/harbor.db"
  elif [[ -f $scratch/restored/harbor.sql ]]; then
    : "${HARBOR_BACKUP_VERIFY_CNF:?Set the separate, isolated verification server client config}"
    : "${HARBOR_BACKUP_VERIFY_DB:?Set an empty isolated verification database}"
    [[ $HARBOR_BACKUP_VERIFY_DB =~ ^harbor_verify_[a-zA-Z0-9_]{1,40}$ ]] || fail 'Use a dedicated harbor_verify_ database'
    [[ $HARBOR_BACKUP_VERIFY_DB != "${HARBOR_BACKUP_MYSQL_DB:-}" ]] || fail 'Verification database must differ from the source'
    [[ $HARBOR_BACKUP_VERIFY_CNF == /* && -r $HARBOR_BACKUP_VERIFY_CNF ]] || fail 'Verification config must be an absolute readable path'
    if [[ -n ${HARBOR_BACKUP_MYSQL_CNF:-} && $HARBOR_BACKUP_VERIFY_CNF -ef $HARBOR_BACKUP_MYSQL_CNF ]]; then
      fail 'Verification requires a separate MySQL client configuration'
    fi
    mysql_verify=(mysql "--defaults-extra-file=$HARBOR_BACKUP_VERIFY_CNF" --batch --skip-column-names
      --max-allowed-packet=64M "$HARBOR_BACKUP_VERIFY_DB")
    tables=$("${mysql_verify[@]}" -e 'SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE();')
    [[ $tables == 0 ]] || fail 'The verification database must be empty'
    "${mysql_verify[@]}" < "$scratch/restored/harbor.sql"
    "${mysql_verify[@]}" -e 'SELECT COUNT(*) AS messages FROM letters; SELECT COUNT(*) AS queue_tasks FROM outbox;'
  else
    fail 'Snapshot contains neither harbor.db nor harbor.sql'
  fi
  printf 'Verified Harbor Mail snapshot %s\n' "$2"
  exit
fi

mkdir -- "$scratch/export"
case "${HARBOR_BACKUP_DRIVER:-}" in
  sqlite)
    : "${HARBOR_BACKUP_SQLITE:?Set an absolute path to the active SQLite database}"
    [[ $HARBOR_BACKUP_SQLITE == /* && -s $HARBOR_BACKUP_SQLITE ]] || fail 'SQLite source is missing or empty'
    command -v sqlite3 >/dev/null || fail 'sqlite3 is required'
    (cd "$scratch/export"; sqlite3 -batch -bail -readonly "$HARBOR_BACKUP_SQLITE" '.timeout 10000' '.backup harbor.db')
    verify_sqlite "$scratch/export/harbor.db"
    artifact=harbor.db
    ;;
  mysql)
    : "${HARBOR_BACKUP_MYSQL_CNF:?Set an absolute path to the backup account client config}"
    : "${HARBOR_BACKUP_MYSQL_DB:?Set the source database name}"
    [[ $HARBOR_BACKUP_MYSQL_CNF == /* && -r $HARBOR_BACKUP_MYSQL_CNF ]] || fail 'Backup config must be an absolute readable path'
    [[ $HARBOR_BACKUP_MYSQL_DB =~ ^[a-zA-Z0-9_]{1,64}$ ]] || fail 'Invalid MySQL database name'
    command -v mysqldump >/dev/null || fail 'mysqldump is required'
    mysqldump "--defaults-extra-file=$HARBOR_BACKUP_MYSQL_CNF" --single-transaction --quick --no-tablespaces --skip-triggers \
      --set-gtid-purged=OFF --hex-blob --max-allowed-packet=64M "$HARBOR_BACKUP_MYSQL_DB" > "$scratch/export/harbor.sql"
    [[ -s $scratch/export/harbor.sql ]] || fail 'MySQL dump is empty'
    artifact=harbor.sql
    ;;
  *) fail 'Set HARBOR_BACKUP_DRIVER to sqlite or mysql' ;;
esac

command -v jq >/dev/null || fail 'jq is required'
(
  cd "$scratch/export"
  sha256sum -- "$artifact" > SHA256SUMS
  restic backup --host "$HARBOR_BACKUP_INSTANCE" --tag "$tag" --json "$artifact" SHA256SUMS > "$scratch/restic.jsonl"
)
snapshot=$(jq -er 'select(.message_type == "summary") | .snapshot_id' "$scratch/restic.jsonl")
[[ $snapshot =~ ^[0-9a-f]{8,64}$ ]] || fail 'Restic returned no snapshot ID'
read_back "$snapshot" "$scratch/restored"
if [[ -n ${HARBOR_BACKUP_METRIC_DIR:-} ]]; then
  [[ $HARBOR_BACKUP_METRIC_DIR == /* && -d $HARBOR_BACKUP_METRIC_DIR ]] || fail 'Invalid metrics directory'
  marker_tmp=$(mktemp -- "$HARBOR_BACKUP_METRIC_DIR/.harbor-backup.XXXXXXXX")
  printf 'harbor_backup_last_success_timestamp_seconds %s\n' "$(date +%s)" > "$marker_tmp"
  chmod 0644 -- "$marker_tmp"
  mv -f -- "$marker_tmp" "$HARBOR_BACKUP_METRIC_DIR/harbor-backup.prom"
  marker_tmp=
fi
printf 'Created and read back Harbor Mail snapshot %s\n' "$snapshot"
