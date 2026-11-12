#!/usr/bin/env bash
# Smoke test for the goredis container image.
#
#   scripts/smoke.sh [image]        (default image: goredis:ci)
#
# Needs only bash, docker and curl: RESP commands are sent over bash's
# /dev/tcp, so redis-cli is not required.
#
# Environment overrides:
#   SMOKE_RESP_PORT   host port for RESP  (default 6379)
#   SMOKE_ADMIN_PORT  host port for admin (default 9379)
set -euo pipefail

IMAGE="${1:-goredis:ci}"
RESP_PORT="${SMOKE_RESP_PORT:-6379}"
ADMIN_PORT="${SMOKE_ADMIN_PORT:-9379}"
HOST=127.0.0.1
NAME="goredis-smoke-$$"
VOLUME="goredis-smoke-data-$$"

# Byte-oriented string handling for RESP lengths.
export LC_ALL=C

log() { printf '[smoke] %s\n' "$*"; }
fail() { printf '[smoke] FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
  local status=$?
  if [ "$status" -ne 0 ]; then
    log "test failed (exit $status); container logs follow"
    docker logs "$NAME" 2>&1 | tail -n 200 || true
    docker inspect --format '{{json .State}}' "$NAME" 2>/dev/null || true
  fi
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker volume rm -f "$VOLUME" >/dev/null 2>&1 || true
  exit "$status"
}
trap cleanup EXIT

# resp_read_reply FD
# Reads one RESP reply from file descriptor FD and prints it in redis-cli
# style: simple string -> text, error -> "(error) text", integer ->
# "(integer) n", null -> "(nil)", bulk string -> its content, array ->
# one element per line. Returns 1 on timeout or malformed input.
resp_read_reply() {
  local fd=$1 line type payload data i
  IFS= read -r -t 5 -u "$fd" line || return 1
  line=${line%$'\r'}
  type=${line:0:1}
  payload=${line:1}
  case "$type" in
    +) printf '%s\n' "$payload" ;;
    -) printf '(error) %s\n' "$payload" ;;
    :) printf '(integer) %s\n' "$payload" ;;
    $)
      if [ "$payload" -lt 0 ]; then
        printf '(nil)\n'
      else
        # Read exactly <len> bytes plus the trailing CRLF.
        IFS= read -r -t 5 -u "$fd" -N $((payload + 2)) data || return 1
        printf '%s\n' "${data:0:payload}"
      fi
      ;;
    \*)
      if [ "$payload" -lt 0 ]; then
        printf '(nil)\n'
      else
        for ((i = 0; i < payload; i++)); do
          resp_read_reply "$fd" || return 1
        done
      fi
      ;;
    *) printf 'unexpected RESP reply: %q\n' "$line" >&2; return 1 ;;
  esac
}

# resp HOST PORT ARG...
# Opens a TCP connection, sends ARG... as one RESP array command, prints the
# reply (see resp_read_reply) and closes the connection.
#   resp 127.0.0.1 6379 SET smoke:key hello   -> OK
resp() {
  local host=$1 port=$2 fd arg cmd rc=0
  shift 2
  cmd="*$#"$'\r\n'
  for arg in "$@"; do
    cmd+="\$${#arg}"$'\r\n'"${arg}"$'\r\n'
  done
  exec {fd}<>"/dev/tcp/${host}/${port}" || return 1
  printf '%s' "$cmd" >&"$fd" || rc=1
  if [ "$rc" -eq 0 ]; then
    resp_read_reply "$fd" || rc=1
  fi
  exec {fd}>&-
  return "$rc"
}

# expect_reply EXPECTED ARG...: run a command and compare the reply.
expect_reply() {
  local expected=$1 got
  shift
  got="$(resp "$HOST" "$RESP_PORT" "$@")" || fail "no reply to: $*"
  [ "$got" = "$expected" ] || fail "$* -> got '$got', want '$expected'"
  log "$* -> $got"
}

wait_ready() {
  local deadline=$((SECONDS + 30))
  until curl -fsS "http://${HOST}:${ADMIN_PORT}/readyz" >/dev/null 2>&1; do
    [ "$(docker inspect --format '{{.State.Running}}' "$NAME")" = "true" ] \
      || fail "container exited before becoming ready"
    [ "$SECONDS" -lt "$deadline" ] || fail "/readyz not 200 after 30s"
    sleep 1
  done
  log "/readyz is 200"
}

wait_healthy() {
  local deadline=$((SECONDS + 60)) status
  while :; do
    status="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$NAME")"
    case "$status" in
      healthy) break ;;
      none) fail "image has no HEALTHCHECK" ;;
    esac
    [ "$SECONDS" -lt "$deadline" ] || fail "docker health status is '$status' after 60s"
    sleep 1
  done
  log "docker health status is healthy"
}

log "image: $IMAGE"
docker volume create "$VOLUME" >/dev/null

# Protected mode is off because the server listens on 0.0.0.0 inside the
# container without a password; the host ports are bound to 127.0.0.1 only.
docker run -d --name "$NAME" \
  -e GOREDIS_PROTECTED_MODE=false \
  -e GOREDIS_SHUTDOWN_TIMEOUT=20s \
  -p "${HOST}:${RESP_PORT}:6379" \
  -p "${HOST}:${ADMIN_PORT}:9379" \
  -v "${VOLUME}:/data" \
  "$IMAGE" >/dev/null

wait_ready
wait_healthy

user="$(docker inspect --format '{{.Config.User}}' "$NAME")"
[ "$user" = "65532:65532" ] || [ "$user" = "65532" ] || fail "container user is '$user', want 65532:65532"
log "container user is $user"

expect_reply PONG PING
expect_reply OK SET smoke:key hello
expect_reply hello GET smoke:key

metrics="$(curl -fsS "http://${HOST}:${ADMIN_PORT}/metrics")" || fail "GET /metrics failed"
grep -q '^goredis_commands_total' <<<"$metrics" || fail "/metrics has no goredis_commands_total"
log "/metrics contains goredis_commands_total"

curl -fsS "http://${HOST}:${ADMIN_PORT}/healthz" >/dev/null || fail "/healthz is not 200"
log "/healthz is 200"

# Graceful stop: SIGTERM, then the server flushes and fsyncs the AOF. The
# timeout is longer than GOREDIS_SHUTDOWN_TIMEOUT so Docker does not SIGKILL.
log "stopping container (SIGTERM)"
docker stop -t 30 "$NAME" >/dev/null
exit_code="$(docker inspect --format '{{.State.ExitCode}}' "$NAME")"
case "$exit_code" in
  0) log "exited with code 0" ;;
  137) fail "container was SIGKILLed (exit 137): graceful shutdown did not finish" ;;
  *) log "WARNING: exit code $exit_code after SIGTERM (expected 0)" ;;
esac

log "starting the same container again (same volume)"
docker start "$NAME" >/dev/null
wait_ready
expect_reply hello GET smoke:key
log "data survived the restart"

log "PASS"
