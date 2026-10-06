#!/bin/sh
# Claude Code "Stop" hook: queue what the assistant answered since your last message, for the hub to show in the chat.
#
#   ~/.claude/settings.json:
#     { "hooks": { "Stop": [ { "hooks": [ { "type": "command", "command": "/path/to/claude-stop-hook.sh" } ] } ] } }
#
# Needs jq and the `agent` binary (on PATH, or set AGENT_BIN). Set CB_STATE_DIR if the agent uses a non-default one.
# The transcript format belongs to Claude Code and can change: this reads lines marked "type":"user" with
# "origin":{"kind":"human"} as your messages and "type":"assistant" lines as the answer.
set -eu
INPUT=$(cat)
TRANSCRIPT=$(printf '%s' "$INPUT" | jq -r '.transcript_path // empty')
[ -n "$TRANSCRIPT" ] && [ -f "$TRANSCRIPT" ] || exit 0

STATE_DIR="${CB_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/claude-bridge}"
MARK="$STATE_DIR/hook-$(basename "$TRANSCRIPT" .jsonl)"
mkdir -p "$STATE_DIR"

TOTAL=$(wc -l < "$TRANSCRIPT" | tr -d ' ')
LAST_HUMAN=$(grep -n '"type":"user"' "$TRANSCRIPT" | grep '"origin":{"kind":"human"}' | tail -1 | cut -d: -f1 || true)
[ -n "$LAST_HUMAN" ] || exit 0

# Turns that end without a new human message (background tasks) must not repeat what was already queued.
START=$LAST_HUMAN
PREV=$(cat "$MARK" 2>/dev/null || echo 0)
if [ "$PREV" -ge "$START" ] 2>/dev/null; then START=$((PREV + 1)); fi

TEXT=$(sed -n "${START},${TOTAL}p" "$TRANSCRIPT" | grep '"type":"assistant"' |
  jq -rs '[.[].message.content[]? | select(.type == "text") | .text] | join("\n\n")' 2>/dev/null || true)
if [ -z "$TEXT" ] || [ "$TEXT" = "null" ]; then
  echo "$TOTAL" > "$MARK"
  exit 0
fi

# Move the mark only after the message is safely in the spool.
printf '%s' "$TEXT" | "${AGENT_BIN:-agent}" send - && echo "$TOTAL" > "$MARK"
exit 0
