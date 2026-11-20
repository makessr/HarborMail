#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

[[ $# == 1 && $1 == /* && -s $1 ]] || {
  printf 'Usage: test-restic-remote.sh /absolute/path/to/harbor.db\n' >&2
  exit 1
}
source_db=$1
work=$(mktemp -d)
server_pid=
cleanup() {
  local status=$?
  trap - EXIT
  if (( status != 0 )) && [[ -s $work/server.log ]]; then
    cat "$work/server.log" >&2
  fi
  if [[ -n $server_pid ]]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -rf -- "$work"
  exit "$status"
}
trap cleanup EXIT

asset=rest-server_0.14.0_linux_amd64
curl --fail --silent --show-error --location --retry 3 --max-time 45 \
  "https://github.com/restic/rest-server/releases/download/v0.14.0/$asset.tar.gz" \
  --output "$work/$asset.tar.gz"
printf '%s  %s\n' 4c9c95bc079a0334e81fad379b19dc5c3353c71c2c88d652cafce2081c2b1c66 "$work/$asset.tar.gz" |
  sha256sum -c -
tar -xzf "$work/$asset.tar.gz" -C "$work" "$asset/rest-server"
chmod 0755 "$work/$asset/rest-server"
port=$(python3 - <<'PY'
import socket
with socket.socket() as listener:
    listener.bind(("127.0.0.1", 0))
    print(listener.getsockname()[1])
PY
)
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -noenc -days 1 \
  -subj /CN=localhost -addext 'subjectAltName=IP:127.0.0.1' \
  -keyout "$work/tls.key" -out "$work/tls.crt"
export RESTIC_REST_USERNAME=harbor-test RESTIC_REST_PASSWORD
RESTIC_REST_PASSWORD=$(openssl rand -hex 24)
printf '%s\n' "$RESTIC_REST_PASSWORD" | htpasswd -niB "$RESTIC_REST_USERNAME" > "$work/htpasswd"
"$work/$asset/rest-server" --listen "127.0.0.1:$port" --path "$work/store" \
  --tls --tls-cert "$work/tls.crt" --tls-key "$work/tls.key" --htpasswd-file "$work/htpasswd" \
  > "$work/server.log" 2>&1 &
server_pid=$!

endpoint="https://127.0.0.1:$port/harbor-test"
status=
for _ in {1..50}; do
  status=$(curl --silent --noproxy '*' --cacert "$work/tls.crt" --max-time 1 \
    --output /dev/null --write-out '%{http_code}' "$endpoint/config" || true)
  [[ $status == 401 ]] && break
  kill -0 "$server_pid"
  sleep .1
done
[[ $status == 401 ]] || { printf 'Expected repository to require authentication\n' >&2; exit 1; }

unset RESTIC_REPOSITORY_FILE RESTIC_PASSWORD RESTIC_PASSWORD_COMMAND
export TZ=UTC NO_PROXY=127.0.0.1 SSL_CERT_FILE="$work/tls.crt"
export RESTIC_REPOSITORY="rest:$endpoint/" RESTIC_PASSWORD_FILE="$work/password" RESTIC_CACHE_DIR="$work/cache"
export HARBOR_BACKUP_WORK="$work/staging" HARBOR_BACKUP_INSTANCE=remote-ci
export HARBOR_BACKUP_DRIVER=sqlite HARBOR_BACKUP_SQLITE="$source_db"
export HARBOR_BACKUP_METRIC_DIR="$work/metrics"
openssl rand -hex 32 > "$RESTIC_PASSWORD_FILE"
mkdir -m 0700 -- "$HARBOR_BACKUP_WORK" "$HARBOR_BACKUP_METRIC_DIR"
restic init
bash ops/backup.sh check
restic --no-lock snapshots --json | jq -e 'length == 0' >/dev/null
[[ ! -e "$HARBOR_BACKUP_METRIC_DIR/harbor-backup.prom" ]]
openssl rand -hex 32 > "$work/wrong-password"
if output=$(RESTIC_PASSWORD_FILE="$work/wrong-password" bash ops/backup.sh check 2>&1); then
  printf 'Preflight accepted a wrong repository password\n' >&2
  exit 1
fi
[[ $output == *'Could not open the Restic repository read-only'* ]]
sqlite3 "$work/incomplete.db" 'CREATE TABLE letters (id INTEGER PRIMARY KEY);'
if output=$(HARBOR_BACKUP_SQLITE="$work/incomplete.db" bash ops/backup.sh check 2>&1); then
  printf 'Preflight accepted an incomplete source schema\n' >&2
  exit 1
fi
[[ $output == *'SQLite source is missing required Harbor Mail tables'* ]]
bash ops/backup.sh backup
[[ -s "$HARBOR_BACKUP_METRIC_DIR/harbor-backup.prom" ]]
snapshot=$(restic snapshots --json | jq -er 'select(length == 1) | .[0].id')
bash ops/backup.sh verify "$snapshot"
bash ops/backup.sh export "$snapshot" "$work/recovered"
(cd "$work/recovered"; sha256sum -c SHA256SUMS)
[[ $(sqlite3 -readonly "$work/recovered/harbor.db" "SELECT count(*) FROM letters WHERE subject='SQLite native service';") == 1 ]]

seed_snapshot() (
  cd "$work"
  printf '%s %s\n' "$1" "$2" > retention-fixture
  restic backup --time "2000-01-01 $2" --host "$1" --tag harbor-mail-remote-ci --json retention-fixture |
    jq -er 'select(.message_type == "summary") | .snapshot_id'
)
# Restic also keeps the oldest snapshot when the requested periods are sparse.
oldest=$(seed_snapshot remote-ci 00:00:00)
expired=$(seed_snapshot remote-ci 00:10:00)
retained=$(seed_snapshot remote-ci 01:00:00)
foreign=$(seed_snapshot unrelated-ci 00:30:00)
snapshot_ids() { restic snapshots --json | jq -ce 'map(.short_id) | sort'; }
before=$(snapshot_ids)
[[ $(jq 'length' <<< "$before") == 5 ]]
bash ops/backup.sh plan
[[ $(snapshot_ids) == "$before" ]]
bash ops/backup.sh prune
snapshot_ids | jq -e --arg expired "${expired:0:8}" --arg retained "${retained:0:8}" \
  --arg oldest "${oldest:0:8}" --arg foreign "${foreign:0:8}" --arg current "${snapshot:0:8}" '
    length == 4 and contains([$oldest, $retained, $foreign, $current]) and (index($expired) == null)
  ' >/dev/null
bash ops/backup.sh verify "$snapshot"
[[ $(restic dump "$snapshot" /harbor-schema) == delivery-timeline-v1 ]]

mkdir -m 0700 -- "$work/legacy"
cp -- "$work/recovered/harbor.db" "$work/legacy/harbor.db"
sqlite3 "$work/legacy/harbor.db" 'DROP TRIGGER delete_outbox_dkim_signature;
  DROP TABLE outbox_dkim_signatures; DROP TABLE domain_dkim_keys;'
legacy_snapshot() (
  cd "$work/legacy"
  sha256sum -- harbor.db > SHA256SUMS
  restic backup --host "$1" --tag "harbor-mail-$1" --json harbor.db SHA256SUMS "${@:2}" |
    jq -er 'select(.message_type == "summary") | .snapshot_id'
)
legacy=$(legacy_snapshot legacy-ci)
HARBOR_BACKUP_INSTANCE=legacy-ci bash ops/backup.sh verify "$legacy"
HARBOR_BACKUP_INSTANCE=legacy-ci bash ops/backup.sh export "$legacy" "$work/legacy-export"
(cd "$work/legacy-export"; sha256sum -c SHA256SUMS)
printf '%s\n' dkim-v1 > "$work/legacy/harbor-schema"
incomplete=$(legacy_snapshot incomplete-ci harbor-schema)
if output=$(HARBOR_BACKUP_INSTANCE=incomplete-ci bash ops/backup.sh export "$incomplete" "$work/incomplete-export" 2>&1); then
  printf 'Export accepted a new-schema snapshot missing DKIM tables.\n' >&2
  exit 1
fi
[[ $output == *'missing required Harbor Mail tables: domain_dkim_keys outbox_dkim_signatures'* ]]
[[ ! -e "$work/incomplete-export" ]]
cp -- "$work/recovered/harbor.db" "$work/legacy/harbor.db"
sqlite3 "$work/legacy/harbor.db" 'DROP TABLE login_audit_events;'
dkim_only=$(legacy_snapshot dkim-only-ci harbor-schema)
HARBOR_BACKUP_INSTANCE=dkim-only-ci bash ops/backup.sh verify "$dkim_only"
printf '%s\n' login-audit-v1 > "$work/legacy/harbor-schema"
missing_audit=$(legacy_snapshot missing-audit-ci harbor-schema)
if output=$(HARBOR_BACKUP_INSTANCE=missing-audit-ci bash ops/backup.sh verify "$missing_audit" 2>&1); then
  printf 'Verification accepted a snapshot missing login audit records.\n' >&2
  exit 1
fi
[[ $output == *'missing required Harbor Mail tables: login_audit_events'* ]]
cp -- "$work/recovered/harbor.db" "$work/legacy/harbor.db"
sqlite3 "$work/legacy/harbor.db" 'DROP TABLE delivery_events;'
login_only=$(legacy_snapshot login-only-ci harbor-schema)
HARBOR_BACKUP_INSTANCE=login-only-ci bash ops/backup.sh verify "$login_only"
printf '%s\n' delivery-timeline-v1 > "$work/legacy/harbor-schema"
missing_timeline=$(legacy_snapshot missing-timeline-ci harbor-schema)
if output=$(HARBOR_BACKUP_INSTANCE=missing-timeline-ci bash ops/backup.sh verify "$missing_timeline" 2>&1); then
  printf 'Verification accepted a new snapshot without delivery events.\n' >&2
  exit 1
fi
[[ $output == *'missing required Harbor Mail tables: delivery_events'* ]]
printf 'Snapshot verification checks the tables required by each schema marker.\n'
printf 'Authenticated HTTPS backup, restore and instance-scoped retention passed.\n'
