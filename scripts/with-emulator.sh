#!/bin/sh
# Run a command with the Pub/Sub emulator available.
# If PUBSUB_EMULATOR_HOST is already set, that emulator is used. Otherwise one is started with gcloud
# (component pubsub-emulator, needs Java) on a free local port and stopped afterwards.
#   scripts/with-emulator.sh go test ./...
set -eu
if [ -n "${PUBSUB_EMULATOR_HOST:-}" ]; then
  exec "$@"
fi
PORT="${PB_EMULATOR_PORT:-18085}"
LOG="$(mktemp -t pb-emulator.XXXXXX)"
gcloud beta emulators pubsub start --host-port="127.0.0.1:$PORT" --project=pb-test >"$LOG" 2>&1 &
PID=$!
cleanup() {
  kill "$PID" 2>/dev/null || true
  pids="$(lsof -ti "tcp:$PORT" 2>/dev/null || true)"
  [ -n "$pids" ] && kill $pids 2>/dev/null || true
  rm -f "$LOG"
}
trap cleanup EXIT INT TERM
i=0
until nc -z 127.0.0.1 "$PORT" 2>/dev/null; do
  i=$((i + 1))
  if [ "$i" -gt 90 ]; then echo "the Pub/Sub emulator did not start:" >&2; cat "$LOG" >&2; exit 1; fi
  sleep 1
done
PUBSUB_EMULATOR_HOST="127.0.0.1:$PORT" "$@"
