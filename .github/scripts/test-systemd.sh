#!/usr/bin/env bash
set -Eeuo pipefail

: "${HARBOR_TEST_MYSQL_DSN:?Set a disposable MySQL DSN}"
: "${HARBOR_TEST_REDIS_URL:?Set a disposable Redis URL}"
if [[ ${RUNNER_ENVIRONMENT:-} != github-hosted ]]; then
  printf 'Run the systemd smoke test on a disposable GitHub-hosted runner.\n' >&2
  exit 1
fi

unit=/etc/systemd/system/harbormail.service
config=/etc/harbormail
binary=/usr/local/bin/harbormail
state=/var/lib/harbormail
for path in "$unit" "$unit.d" "$config" "$binary" "$state"; do
  if sudo test -e "$path" || sudo test -L "$path"; then
    printf 'Smoke test path already exists: %s\n' "$path" >&2
    exit 1
  fi
done
if getent passwd harbormail >/dev/null; then
  printf 'Smoke test user already exists.\n' >&2
  exit 1
fi

work=$(mktemp -d)
cleanup() {
  local status=$?
  trap - EXIT
  if sudo test -e "$unit"; then
    if (( status != 0 )); then sudo journalctl -u harbormail.service --no-pager -n 50 >&2 || true; fi
    sudo systemctl stop harbormail.service || true
    sudo systemctl clean --what=state harbormail.service || true
  fi
  rm -rf -- "$work"
  exit "$status"
}
trap cleanup EXIT
umask 077

go build -trimpath -o "$work/harbormail" ./cmd/harbormail
password=$(openssl rand -hex 24)
printf '::add-mask::%s\n' "$password"
hash=$(printf '%s\n' "$password" | "$work/harbormail" hash-password)
printf '{"probe@harbor.test":"%s"}\n' "$hash" > "$work/accounts.json"
read -r web_port smtp_port < <(python3 - <<'PY'
import socket
with socket.socket() as web, socket.socket() as smtp:
    web.bind(("127.0.0.1", 0))
    smtp.bind(("127.0.0.1", 0))
    print(web.getsockname()[1], smtp.getsockname()[1])
PY
)
cp ops/harbormail.env.example "$work/harbormail.env"
printf '\nHARBOR_WEB=127.0.0.1:%s\nHARBOR_SMTP=127.0.0.1:%s\n' "$web_port" "$smtp_port" >> "$work/harbormail.env"
sudo useradd --system --user-group --home-dir "$state" --shell "$(command -v nologin)" harbormail
sudo install -d -m 0700 "$config"
sudo install -m 0600 "$work/accounts.json" "$work/harbormail.env" "$config/"
sudo install -m 0755 "$work/harbormail" "$binary"
sudo install -m 0644 ops/harbormail.service "$unit"
sudo systemd-analyze verify "$unit"
sudo systemctl daemon-reload

wait_for_web() {
  local attempt pid
  for attempt in {1..50}; do
    if curl -fsS --max-time 2 "http://127.0.0.1:$web_port/healthz" >/dev/null 2>&1; then
      pid=$(systemctl show --property MainPID --value harbormail.service)
      [[ $(ps -o euid= -p "$pid" | tr -d ' ') == "$(id -u harbormail)" ]]
      return
    fi
    sleep .2
  done
  printf 'Native Web health endpoint did not become ready.\n' >&2
  return 1
}

check_inbox() {
  HARBOR_SMOKE_PASSWORD=$password python3 - "$web_port" "$smtp_port" "$1" "$2" "${3:-}" <<'PY'
import http.cookiejar
import json
import os
import smtplib
import socket
import sys
import urllib.parse
import urllib.request
from email.message import EmailMessage

web_port, smtp_port, subject, action, check_backend = sys.argv[1:]
address = "probe@harbor.test"
if action == "send":
    message = EmailMessage()
    message["From"] = "sender@example.test"
    message["To"] = address
    message["Subject"] = subject
    message.set_content("systemd smoke test")
    with smtplib.SMTP("127.0.0.1", int(smtp_port), timeout=5) as relay:
        relay.send_message(message)

base = f"http://127.0.0.1:{web_port}"
browser = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
data = json.dumps({"email": address, "password": os.environ["HARBOR_SMOKE_PASSWORD"]}).encode()
with browser.open(urllib.request.Request(base + "/api/login", data, {"Content-Type": "application/json"}), timeout=5):
    pass
with browser.open(base + "/api/letters?folder=inbox", timeout=5) as response:
    messages = json.load(response)["letters"]
if sum(item["subject"] == subject for item in messages) != 1:
    raise RuntimeError(f"Expected exactly one test message for {subject}")

if check_backend == "redis":
    url = urllib.parse.urlsplit(os.environ["HARBOR_TEST_REDIS_URL"])
    for _ in range(3):
        with browser.open(base + "/api/admin/overview", timeout=5) as response:
            json.load(response)
        with socket.create_connection((url.hostname, url.port), timeout=3) as redis:
            redis.sendall(b"EXISTS harbor-mail:systemd-smoke:mysql:admin:v1:overview\r\n")
            with redis.makefile("rb") as reply:
                if reply.readline() == b":1\r\n":
                    break
    else:
        raise RuntimeError("Native MySQL deployment did not populate the Redis overview cache")
PY
}

sudo systemctl start harbormail.service
wait_for_web
[[ $(sudo stat -c '%a:%U' "$state") == 700:harbormail ]]
[[ $(sudo stat -c '%a:%U' "$state/harbor.db") == 600:harbormail ]]
check_inbox "SQLite native service" send
sudo systemctl restart harbormail.service
wait_for_web
check_inbox "SQLite native service" read
sudo systemctl stop harbormail.service
[[ $(systemctl show --property Result --value harbormail.service) == success ]]
printf 'SQLite systemd service passed SMTP, Web, restart, and file permission checks.\n'

printf '%s' "$HARBOR_TEST_MYSQL_DSN" > "$work/mysql-dsn"
printf '%s' "$HARBOR_TEST_REDIS_URL" > "$work/redis-url"
sudo install -m 0600 "$work/mysql-dsn" "$work/redis-url" "$config/"
printf '\nHARBOR_DB_DRIVER=mysql\nHARBOR_REDIS_NAMESPACE=systemd-smoke\nHARBOR_ADMINS=probe@harbor.test\n' >> "$work/harbormail.env"
sudo install -m 0600 "$work/harbormail.env" "$config/harbormail.env"
sudo install -d -m 0755 "$unit.d"
sudo tee "$unit.d/backends.conf" >/dev/null <<'UNIT'
[Service]
LoadCredential=mysql-dsn:/etc/harbormail/mysql-dsn
LoadCredential=redis-url:/etc/harbormail/redis-url
Environment=HARBOR_MYSQL_DSN_FILE=%d/mysql-dsn
Environment=HARBOR_REDIS_URL_FILE=%d/redis-url
UNIT
sudo systemctl daemon-reload
sudo systemctl start harbormail.service
wait_for_web
check_inbox "MySQL native service" send redis
sudo systemctl restart harbormail.service
wait_for_web
check_inbox "MySQL native service" read
sudo systemctl stop harbormail.service
[[ $(systemctl show --property Result --value harbormail.service) == success ]]
printf 'MySQL systemd service passed SMTP, Web, restart, and Redis cache checks.\n'
