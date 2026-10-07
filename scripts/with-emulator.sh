#!/bin/sh
# Run a command with the Pub/Sub emulator available.
# If PUBSUB_EMULATOR_HOST is already set, that emulator is used. Otherwise one is started with gcloud
# (component pubsub-emulator, needs Java) on a local port (PB_EMULATOR_PORT, default 18085) and stopped afterwards.
# If something already listens on that port the script refuses to start: it would otherwise run the command against
# a stranger's listener (a developer's own emulator, say) and then stop it. Set PUBSUB_EMULATOR_HOST to use that one
# on purpose, or PB_EMULATOR_PORT to use another port.
#   scripts/with-emulator.sh go test ./...
set -eu
if [ -n "${PUBSUB_EMULATOR_HOST:-}" ]; then
  exec "$@"
fi
PORT="${PB_EMULATOR_PORT:-18085}"
if nc -z 127.0.0.1 "$PORT" 2>/dev/null; then
  echo "port $PORT is already in use: not starting an emulator and not running the command against a listener that is not mine." >&2
  echo "Set PUBSUB_EMULATOR_HOST=127.0.0.1:$PORT to use it, or PB_EMULATOR_PORT to use another port." >&2
  exit 1
fi
LOG="$(mktemp -t pb-emulator.XXXXXX)"
gcloud beta emulators pubsub start --host-port="127.0.0.1:$PORT" --project=pb-test >"$LOG" 2>&1 &
PID=$!
# Stop the emulator that this script started: its process and everything below it (gcloud starts the Java emulator as a
# child). Whatever else may listen on the port is not touched.
kill_tree() {
  for child in $(pgrep -P "$1" 2>/dev/null || true); do kill_tree "$child"; done
  kill "$1" 2>/dev/null || true
}
cleanup() {
  kill_tree "$PID"
  rm -f "$LOG"
}
trap cleanup EXIT INT TERM
i=0
until nc -z 127.0.0.1 "$PORT" 2>/dev/null; do
  # A listener counts as my emulator only while the process I started is alive.
  if ! kill -0 "$PID" 2>/dev/null; then echo "the Pub/Sub emulator exited before it listened:" >&2; cat "$LOG" >&2; exit 1; fi
  i=$((i + 1))
  if [ "$i" -gt 90 ]; then echo "the Pub/Sub emulator did not start:" >&2; cat "$LOG" >&2; exit 1; fi
  sleep 1
done
# Check once more: a stranger may have taken the port while my process was dying.
if ! kill -0 "$PID" 2>/dev/null; then echo "the Pub/Sub emulator exited, and something else now listens on port $PORT:" >&2; cat "$LOG" >&2; exit 1; fi
PUBSUB_EMULATOR_HOST="127.0.0.1:$PORT" "$@"
