#!/bin/sh
# Claude Code "Stop" hook: queue what the assistant answered since your last message, for the hub to show in the chat.
#
#   ~/.claude/settings.json:
#     { "hooks": { "Stop": [ { "hooks": [ { "type": "command", "command": "/path/to/claude-stop-hook.sh" } ] } ] } }
#
# Needs jq and the `agent` binary (on PATH, or set AGENT_BIN). Set PB_STATE_DIR if the agent uses a non-default one.
# The transcript format belongs to Claude Code and can change: this reads lines marked "type":"user" with
# "origin":{"kind":"human"} as your messages and "type":"assistant" lines as the answer.
set -eu
INPUT=$(cat)
TRANSCRIPT=$(printf '%s' "$INPUT" | jq -r '.transcript_path // empty')
[ -n "$TRANSCRIPT" ] && [ -f "$TRANSCRIPT" ] || exit 0

STATE_DIR="${PB_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/petrvs-bridge}"
MARK="$STATE_DIR/hook-$(basename "$TRANSCRIPT" .jsonl)"
mkdir -p "$STATE_DIR"

LAST_HUMAN=$(grep -n '"type":"user"' "$TRANSCRIPT" | grep '"origin":{"kind":"human"}' | tail -1 | cut -d: -f1 || true)
[ -n "$LAST_HUMAN" ] || exit 0

# Turns that end without a new human message (background tasks) must not repeat what was already queued.
START=$LAST_HUMAN
PREV=$(cat "$MARK" 2>/dev/null || echo 0)
if [ "$PREV" -ge "$START" ] 2>/dev/null; then START=$((PREV + 1)); fi

# Claude Code can run this hook before it has written the final answer to the transcript, so look again for a few
# seconds. Words written before a tool call are a text block too, and they are in the file long before the answer that
# follows the tool: taking them for the whole answer queues only them and the turn is marked done. So the text counts as
# complete only when the last assistant line is not an unfinished message (stop_reason "tool_use") and holds text. If that
# never happens within the wait (an interrupted turn), what is there is sent. Nothing is marked when no text is found:
# the next run looks at the same lines again.
TEXT=""
TOTAL=0
for _ in 1 2 3 4 5 6; do
  TOTAL=$(wc -l < "$TRANSCRIPT" | tr -d ' ')
  LINES=$(sed -n "${START},${TOTAL}p" "$TRANSCRIPT" | grep '"type":"assistant"' || true)
  TEXT=$(printf '%s\n' "$LINES" |
    jq -rs '[.[].message.content[]? | select(.type == "text") | .text] | join("\n\n")' 2>/dev/null || true)
  if [ -n "$TEXT" ] && [ "$TEXT" != "null" ]; then
    DONE=$(printf '%s\n' "$LINES" | jq -rs 'last | if .message.stop_reason == "tool_use" or ([.message.content[]? | select(.type == "text")] | length) == 0 then "no" else "yes" end' 2>/dev/null || true)
    [ "$DONE" = "yes" ] && break
  fi
  sleep 0.5
done
if [ -z "$TEXT" ] || [ "$TEXT" = "null" ]; then exit 0; fi

# Move the mark only after the message is safely in the spool.
printf '%s' "$TEXT" | "${AGENT_BIN:-agent}" send - && echo "$TOTAL" > "$MARK"
exit 0
