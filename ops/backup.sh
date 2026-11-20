#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

fail() { printf 'Harbor Mail backup: %s\n' "$*" >&2; exit 1; }

mode=${1:-}
case "$mode" in
  backup|check|prune|plan) [[ $# == 1 ]] || fail "Usage: backup.sh $mode" ;;
  verify) [[ $# == 2 && $2 =~ ^[0-9a-f]{8,64}$ ]] || fail 'Usage: backup.sh verify SNAPSHOT_ID' ;;
  export) [[ $# == 3 && $2 =~ ^[0-9a-f]{8,64}$ && $3 == /* ]] ||
    fail 'Usage: backup.sh export SNAPSHOT_ID /absolute/new-directory' ;;
  *) fail 'Usage: backup.sh {backup|check|plan|prune|verify SNAPSHOT_ID|export SNAPSHOT_ID /absolute/new-directory}' ;;
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
current_schema=delivery-timeline-v1
exec 9>"$HARBOR_BACKUP_WORK/$tag.lock"
flock -n 9 || fail 'Another backup or verification is running'

if [[ $mode == plan || $mode == prune ]]; then
  options=(--host "$HARBOR_BACKUP_INSTANCE" --tag "$tag" --group-by tags --keep-daily 14 --keep-weekly 4)
  if [[ $mode == plan ]]; then options+=(--dry-run); else options+=(--prune); fi
  restic forget "${options[@]}"
  exit
fi

command -v jq >/dev/null || fail 'jq is required'
work_root=$(realpath -- "$HARBOR_BACKUP_WORK")
scratch=$(mktemp -d -- "$work_root/harbor.XXXXXXXX")
[[ $(realpath -- "$scratch") == "$work_root"/harbor.* ]] || fail 'Invalid temporary backup directory'
marker_tmp=
restore_stage=
cleanup() {
  local result=$?
  trap - EXIT
  if [[ -n $marker_tmp ]]; then rm -f -- "$marker_tmp"; fi
  if [[ -n $restore_stage ]]; then rm -rf -- "$restore_stage"; fi
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
  verify_sqlite_tables "$1" 'SQLite snapshot' "$2"
}

verify_sqlite_tables() {
  local available
  available=$(sqlite3 -batch -bail -readonly "$1" '.timeout 10000' "SELECT name FROM sqlite_master WHERE type='table';") ||
    fail "Could not read $2 tables"
  verify_required_tables "$2" "$available" "$3"
}

verify_required_tables() {
  local artifact=$1 available=$2 schema=$3 table
  shift 3
  # Required tables from the first public schema. Keep the list compatible
  # with existing complete snapshots as newer releases add tables.
  local -a required=(
    account_access account_password_overrides admin_account_actions admin_alias_actions
    admin_delivery_resolutions admin_delivery_retries admin_mfa_used app_passwords
    contacts custom_folders draft_creation_keys imap_deleted imap_uid_state imap_uids
    letters mail_label_links mail_labels mail_rules mail_snoozes mail_templates mail_threads
    mailbox_preferences mailbox_quota mailbox_quota_meta mailbox_retirements
    mailbox_send_delay mailbox_storage_alerts managed_accounts managed_aliases managed_domains
    outbox outbox_recipient_progress outbox_schedule saved_searches sessions signature_settings
    spam_feedback web_drafts web_letter_recipients web_outbox_envelopes web_session_details
  )
  if [[ $schema != legacy ]] || grep -Eq '^(domain_dkim_keys|outbox_dkim_signatures)$' <<< "$available"; then
    required+=(domain_dkim_keys outbox_dkim_signatures)
  fi
  if [[ $schema == "$current_schema" || $schema == login-audit-v1 ]]; then
    required+=(login_audit_events)
  fi
  if [[ $schema == "$current_schema" ]]; then
    required+=(delivery_events)
  fi
  local -a missing=()
  for table in "${required[@]}" "$@"; do
    if ! grep -Fxq -- "$table" <<< "$available"; then
      missing+=("$table")
    fi
  done
  if (( ${#missing[@]} > 0 )); then
    fail "$artifact is missing required Harbor Mail tables: ${missing[*]}. Check the source database and backup access."
  fi
}

verify_mysql_dump() {
  local dump=$1 available
  [[ -s $dump ]] || fail 'MySQL dump is empty'
  # mysqldump can exit successfully while omitting tables invisible to its account.
  available=$(sed -nE 's/^CREATE TABLE (IF NOT EXISTS )?`([a-z_]+)`[[:space:]]*\(.*/\2/p' "$dump") ||
    fail 'Could not read MySQL dump tables'
  verify_required_tables 'MySQL dump' "$available" "$2" mailbox_revisions
}

snapshot_schema() {
  local marker="$1/harbor-schema" schema
  if [[ ! -e $marker && ! -L $marker ]]; then
    printf 'legacy\n'
    return
  fi
  [[ -f $marker && ! -L $marker ]] || fail 'Snapshot has an invalid schema marker'
  for schema in "$current_schema" login-audit-v1 dkim-v1; do
    if printf '%s\n' "$schema" | cmp -s -- - "$marker"; then
      printf '%s\n' "$schema"
      return
    fi
  done
  fail 'Snapshot has an unsupported or invalid schema marker'
}

read_back() (
  local snapshot=$1 destination=$2
  local artifact checksum
  restic restore "$snapshot" --target "$destination" --verify \
    --include /harbor.db --include /harbor.sql --include /SHA256SUMS --include /harbor-schema >&2 ||
    fail 'Could not read back the snapshot'
  cd "$destination" || fail 'Could not open the restored snapshot directory'
  if [[ -f harbor.db && ! -L harbor.db && ! -e harbor.sql && ! -L harbor.sql ]]; then
    artifact=harbor.db
  elif [[ -f harbor.sql && ! -L harbor.sql && ! -e harbor.db && ! -L harbor.db ]]; then
    artifact=harbor.sql
  else
    fail 'Snapshot must contain exactly one Harbor Mail database artifact'
  fi
  [[ -f SHA256SUMS && ! -L SHA256SUMS ]] || fail 'Snapshot does not contain a regular checksum manifest'
  checksum=$(sha256sum -- "$artifact") || fail 'Could not hash the restored database artifact'
  [[ $(<SHA256SUMS) == "$checksum" ]] || fail "Snapshot checksum does not match $artifact"
  printf '%s\n' "$artifact"
)

if [[ $mode == verify || $mode == export ]]; then
  restored="$scratch/restored"
  if [[ $mode == export ]]; then
    restore_name=$(basename -- "$3")
    [[ $restore_name != / && $restore_name != . && $restore_name != .. ]] || fail 'Invalid restore directory name'
    restore_parent=$(realpath -e -- "$(dirname -- "$3")") || fail 'Restore parent directory must exist'
    [[ -d $restore_parent ]] || fail 'Restore parent must be a directory'
    destination="${restore_parent%/}/$restore_name"
    [[ ! -e $destination && ! -L $destination ]] || fail 'Restore destination already exists'
  fi
  snapshot=$(restic snapshots --json "$2" |
    jq -er --arg instance "$HARBOR_BACKUP_INSTANCE" --arg tag "$tag" '
      select(length == 1)
      | .[0]
      | select(.hostname == $instance and ((.tags // []) | index($tag) != null))
      | .id
    ') || fail 'Snapshot not found or does not belong to this Harbor Mail instance'
  [[ $snapshot =~ ^[0-9a-f]{64}$ ]] || fail 'Restic returned an invalid snapshot ID'
  if [[ $mode == export ]]; then
    restore_stage=$(mktemp -d -- "$restore_parent/.harbor-restore.XXXXXXXX")
    restored="$restore_stage/snapshot"
  fi
  artifact=$(read_back "$snapshot" "$restored")
  schema=$(snapshot_schema "$restored")
  if [[ $artifact == harbor.db ]]; then
    verify_sqlite "$restored/harbor.db" "$schema"
  else
    verify_mysql_dump "$restored/harbor.sql" "$schema"
  fi
  if [[ $mode == export ]]; then
    publish="$restore_stage/publish"
    mkdir -m 0700 -- "$publish"
    chmod 0600 -- "$restored/$artifact" "$restored/SHA256SUMS"
    mv -- "$restored/$artifact" "$restored/SHA256SUMS" "$publish/"
    # Publish both files together on the destination filesystem.
    mv -nT -- "$publish" "$destination" || fail 'Could not publish restore directory'
    [[ ! -d $publish ]] || fail 'Restore destination already exists'
    printf 'Exported Harbor Mail snapshot %s to %s\n' "$snapshot" "$destination"
    exit
  fi
  if [[ $artifact == harbor.sql ]]; then
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
    "${mysql_verify[@]}" < "$restored/harbor.sql"
    "${mysql_verify[@]}" -e 'SELECT COUNT(*) AS messages FROM letters; SELECT COUNT(*) AS queue_tasks FROM outbox;'
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
    if [[ $mode == check ]]; then
      verify_sqlite_tables "$HARBOR_BACKUP_SQLITE" 'SQLite source' "$current_schema"
    else
      (cd "$scratch/export"; sqlite3 -batch -bail -readonly "$HARBOR_BACKUP_SQLITE" '.timeout 10000' '.backup harbor.db')
      verify_sqlite "$scratch/export/harbor.db" "$current_schema"
      artifact=harbor.db
    fi
    ;;
  mysql)
    : "${HARBOR_BACKUP_MYSQL_CNF:?Set an absolute path to the backup account client config}"
    : "${HARBOR_BACKUP_MYSQL_DB:?Set the source database name}"
    [[ $HARBOR_BACKUP_MYSQL_CNF == /* && -r $HARBOR_BACKUP_MYSQL_CNF ]] || fail 'Backup config must be an absolute readable path'
    [[ $HARBOR_BACKUP_MYSQL_DB =~ ^[a-zA-Z0-9_]{1,64}$ ]] || fail 'Invalid MySQL database name'
    command -v mysqldump >/dev/null || fail 'mysqldump is required'
    schema_only=()
    if [[ $mode == check ]]; then schema_only=(--no-data --skip-lock-tables); fi
    mysqldump "--defaults-extra-file=$HARBOR_BACKUP_MYSQL_CNF" --single-transaction --quick --no-tablespaces --skip-triggers \
      --set-gtid-purged=OFF --hex-blob --max-allowed-packet=64M "${schema_only[@]}" "$HARBOR_BACKUP_MYSQL_DB" > "$scratch/export/harbor.sql"
    verify_mysql_dump "$scratch/export/harbor.sql" "$current_schema"
    artifact=harbor.sql
    ;;
  *) fail 'Set HARBOR_BACKUP_DRIVER to sqlite or mysql' ;;
esac

if [[ $mode == check ]]; then
  if [[ -n ${HARBOR_BACKUP_METRIC_DIR:-} ]]; then
    [[ $HARBOR_BACKUP_METRIC_DIR == /* && -d $HARBOR_BACKUP_METRIC_DIR && -w $HARBOR_BACKUP_METRIC_DIR && -x $HARBOR_BACKUP_METRIC_DIR ]] ||
      fail 'Metrics directory must exist and be writable by the backup user'
  fi
  restic --no-lock --no-cache cat config >/dev/null 2>&1 ||
    fail 'Could not open the Restic repository read-only; check its address, access credentials, TLS and repository password'
  printf 'Harbor Mail backup preflight passed: source schema and Restic read access.\n'
  exit
fi

(
  cd "$scratch/export"
  sha256sum -- "$artifact" > SHA256SUMS
  printf '%s\n' "$current_schema" > harbor-schema
  restic backup --host "$HARBOR_BACKUP_INSTANCE" --tag "$tag" --json "$artifact" SHA256SUMS harbor-schema > "$scratch/restic.jsonl"
)
snapshot=$(jq -er 'select(.message_type == "summary") | .snapshot_id' "$scratch/restic.jsonl")
[[ $snapshot =~ ^[0-9a-f]{8,64}$ ]] || fail 'Restic returned no snapshot ID'
restored_artifact=$(read_back "$snapshot" "$scratch/restored")
[[ $restored_artifact == "$artifact" ]] ||
  fail 'Read-back artifact does not match the database backup'
read_schema=$(snapshot_schema "$scratch/restored")
[[ $read_schema == "$current_schema" ]] || fail 'Read-back snapshot is missing the schema marker'
if [[ -n ${HARBOR_BACKUP_METRIC_DIR:-} ]]; then
  [[ $HARBOR_BACKUP_METRIC_DIR == /* && -d $HARBOR_BACKUP_METRIC_DIR ]] || fail 'Invalid metrics directory'
  marker_tmp=$(mktemp -- "$HARBOR_BACKUP_METRIC_DIR/.harbor-backup.XXXXXXXX")
  printf 'harbor_backup_last_success_timestamp_seconds %s\n' "$(date +%s)" > "$marker_tmp"
  chmod 0644 -- "$marker_tmp"
  mv -fT -- "$marker_tmp" "$HARBOR_BACKUP_METRIC_DIR/harbor-backup.prom"
  marker_tmp=
fi
printf 'Created and read back Harbor Mail snapshot %s\n' "$snapshot"
