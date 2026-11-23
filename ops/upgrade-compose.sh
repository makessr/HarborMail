#!/usr/bin/env bash
# Upgrade a single Harbor Mail Compose service backed by the project's MySQL container.
set -Eeuo pipefail
umask 077

usage() {
  printf 'Usage: %s [--check] VERSION HEALTH_URL\n' "${0##*/}"
  printf 'Run from the production Compose directory. Optional: HARBOR_UPGRADE_PROJECT, HARBOR_UPGRADE_BACKUP_DIR.\n'
}

die() {
  printf 'Harbor upgrade: %s\n' "$*" >&2
  exit 1
}

check_only=false
case ${1:-} in
  --help|-h) usage; exit 0 ;;
  --check) check_only=true; shift ;;
esac
[[ $# == 2 ]] || { usage >&2; exit 2; }
version=$1
health_url=$2
[[ $version =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || die 'Expected a stable version such as v0.2.15'
[[ $health_url == https://*/healthz ]] || die 'Expected the HTTPS /healthz URL'
project=${HARBOR_UPGRADE_PROJECT:-harbor-mail}
backup_root=${HARBOR_UPGRADE_BACKUP_DIR:-/root}
latest=ghcr.io/makessr/harbormail:latest

workdir=$(pwd -P)
override=$workdir/compose.server.yaml
[[ -f $override && ! -L $override && -r .env && -d secrets ]] ||
  die 'Expected compose.server.yaml, .env and secrets/ in the deployment directory'

exec 9> .harbor-upgrade.lock
flock -n 9 || die 'An upgrade is already running in this directory'

running_container() {
  local service=$1 ids
  ids=$(docker ps -q --filter "label=com.docker.compose.project=$project" \
    --filter "label=com.docker.compose.service=$service") || return 1
  [[ $ids =~ ^[a-f0-9]{12,64}$ ]] || return 1
  printf '%s\n' "$ids"
}

app=$(running_container harbor) || die 'Expected exactly one running Harbor container'
mysql=$(running_container mysql) || die 'Expected exactly one running MySQL container'
live_dir=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project.working_dir"}}' "$app")
[[ $live_dir == "$workdir" ]] || die "Running deployment uses $live_dir"
file_list=$(docker inspect -f '{{index .Config.Labels "com.docker.compose.project.config_files"}}' "$app")
IFS=, read -r -a files <<< "$file_list"
[[ ${#files[@]} -gt 1 && ${files[-1]} == "$override" ]] || die 'compose.server.yaml must be the last active Compose file'

compose=(docker compose --project-directory "$workdir" -p "$project")
config_paths=(.env secrets)
for file in "${files[@]}"; do
  [[ $file == "$workdir/"* && -f $file && ! -L $file ]] ||
    die 'Compose files must be regular files inside the deployment directory'
  config_paths+=("${file#"$workdir/"}")
  compose+=(-f "$file")
done
previous_image=$(docker inspect -f '{{.Config.Image}}' "$app")
previous_id=$(docker inspect -f '{{.Image}}' "$app")
[[ $previous_image == "$latest" ]] || die 'The running Harbor container must use ghcr.io/makessr/harbormail:latest'
[[ $previous_id =~ ^sha256:[a-f0-9]{64}$ && $(docker image inspect -f '{{.Id}}' "$previous_id") == "$previous_id" ]] ||
  die 'The previous image is unavailable for rollback'
previous_version=$(docker exec "$app" /usr/local/bin/harbormail version)
data_dir=$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/var/lib/harbormail"}}{{.Source}}{{end}}{{end}}' "$app")
[[ $data_dir == /* && -d $data_dir && $data_dir != / ]] || die 'Unable to locate the Harbor data mount on this host'

health() {
  local container=$1
  [[ $(docker inspect -f '{{.State.Health.Status}}' "$container") == healthy ]] &&
    [[ $(curl -fsS --retry 3 --retry-delay 2 --max-time 12 "$health_url") == ok ]]
}
health "$app" || die 'The deployed service is unhealthy'
resolved_image() {
  "$@" config --format json |
    python3 -c 'import json, sys; print(json.load(sys.stdin)["services"]["harbor"]["image"])'
}
[[ $(resolved_image "${compose[@]}") == "$previous_image" ]] ||
  die 'The running image and the current Compose configuration differ'

if $check_only; then
  printf 'Preflight passed. Running %s (%s); upgrade will pull %s and expect %s.\n' "$previous_version" "$previous_id" "$latest" "$version"
  exit 0
fi

backup=
recreate_started=false
rollback_image="harbormail-rollback:${previous_id#sha256:}"
recreate=(up -d --no-deps --no-build --pull never --force-recreate --wait --wait-timeout 120 harbor)
rollback() {
  trap - ERR HUP INT TERM
  printf 'Upgrade failed. Restoring %s to the previous image.\n' "$latest" >&2
  local restored
  if [[ $(docker image inspect -f '{{.Id}}' "$rollback_image") == "$previous_id" ]] &&
     docker tag "$rollback_image" "$latest" &&
     { ! $recreate_started ||
       { cmp -s "$override" "$backup/compose.server.yaml" && "${compose[@]}" "${recreate[@]}"; }; } &&
     restored=$(running_container harbor) &&
     [[ $(docker inspect -f '{{.Image}}' "$restored") == "$previous_id" ]] &&
     health "$restored"; then
    printf 'Previous image %s (%s) is healthy.\n' "$previous_version" "$previous_id" >&2
  else
    printf 'Rollback requires manual recovery. Preserved image: %s\n' "$rollback_image" >&2
  fi
  if [[ -n $backup ]]; then
    printf 'Upgrade backup: %s\n' "$backup" >&2
  fi
  exit 1
}

docker tag "$previous_id" "$rollback_image"
trap 'if (( BASH_SUBSHELL == 0 )); then rollback; fi' ERR
trap rollback HUP INT TERM
docker pull "$latest"
candidate_id=$(docker image inspect -f '{{.Id}}' "$latest")
candidate_version=$(docker run --rm --network none "$candidate_id" version)
if [[ $candidate_version != "$version" ]]; then
  printf 'Expected %s, but %s reports %s.\n' "$version" "$latest" "$candidate_version" >&2
  rollback
fi
if [[ $candidate_id == "$previous_id" ]]; then
  trap - ERR HUP INT TERM
  printf 'Already running %s (%s) and healthy.\n' "$version" "$candidate_id"
  exit 0
fi

backup=$(mktemp -d "$backup_root/harbor-upgrade-$version.XXXXXXXX")
printf 'Saving upgrade backup in %s\n' "$backup"
tar -czf "$backup/config.tar.gz" -- "${config_paths[@]}"
tar -czf "$backup/harbor-data.tar.gz" -C "$data_dir" .
docker exec "$mysql" sh -eu -c '
  test -n "$MYSQL_DATABASE"
  MYSQL_PWD="$(cat "$MYSQL_ROOT_PASSWORD_FILE")"; export MYSQL_PWD
  exec mysqldump --user=root --single-transaction --quick --no-tablespaces \
    --set-gtid-purged=OFF --hex-blob --max-allowed-packet=64M --skip-triggers "$MYSQL_DATABASE"
' | gzip -c > "$backup/mysql.sql.gz"
gzip -dc "$backup/mysql.sql.gz" |
  awk 'index($0,"CREATE TABLE `letters`") == 1 { found = 1 } END { exit !found }' ||
  { printf 'The MySQL dump is missing the letters table.\n' >&2; rollback; }
tar -tzf "$backup/config.tar.gz" >/dev/null
tar -tzf "$backup/harbor-data.tar.gz" >/dev/null
cp -p -- "$override" "$backup/compose.server.yaml"
printf '%s\n' "$rollback_image" > "$backup/previous-image"
printf '%s\n' "$previous_id" > "$backup/previous-image-id"
(cd "$backup"; sha256sum config.tar.gz harbor-data.tar.gz mysql.sql.gz compose.server.yaml previous-image previous-image-id > SHA256SUMS; sha256sum -c SHA256SUMS)

cmp -s "$override" "$backup/compose.server.yaml"
[[ $(resolved_image "${compose[@]}") == "$latest" ]]
[[ $(running_container harbor) == "$app" ]]
[[ $(docker image inspect -f '{{.Id}}' "$latest") == "$candidate_id" ]]
recreate_started=true
"${compose[@]}" "${recreate[@]}"
app=$(running_container harbor)
[[ $(docker inspect -f '{{.Config.Image}}' "$app") == "$latest" ]]
[[ $(docker inspect -f '{{.Image}}' "$app") == "$candidate_id" ]]
[[ $(docker exec "$app" /usr/local/bin/harbormail version) == "$version" ]]
health "$app"
trap - ERR HUP INT TERM
printf 'Harbor Mail %s (%s) is healthy. Upgrade backup: %s\n' "$version" "$candidate_id" "$backup"
