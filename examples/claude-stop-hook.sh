#!/bin/sh
# Claude Code "Stop" hook: queue what the assistant answered since your last message, for the hub to show in the chat.
#
#   ~/.claude/settings.json:
#     { "hooks": { "Stop": [ { "hooks": [ { "type": "command", "command": "/path/to/claude-stop-hook.sh" } ] } ] } }
#
# Needs jq and the `agent` binary (on PATH, or set AGENT_BIN). Set PB_STATE_DIR if the agent uses a non-default one.
# Pictures that Claude read during the turn are sent to the chat after the answer (see the end); PB_FORWARD_IMAGES=off stops that.
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
  # claude-pretool-hook.sh runs this script with PB_EARLY set, before a command that may take the machine down: the turn
  # goes on after this run, so what is written so far is all there is to take, and there is no final answer to wait for.
  [ -z "${PB_EARLY:-}" ] || break
  if [ -n "$TEXT" ] && [ "$TEXT" != "null" ]; then
    DONE=$(printf '%s\n' "$LINES" | jq -rs 'last | if .message.stop_reason == "tool_use" or ([.message.content[]? | select(.type == "text")] | length) == 0 then "no" else "yes" end' 2>/dev/null || true)
    [ "$DONE" = "yes" ] && break
  fi
  sleep 0.5
done
if [ -z "$TEXT" ] || [ "$TEXT" = "null" ]; then exit 0; fi

# Move the mark only after the message is safely in the spool.
if printf '%s' "$TEXT" | "${AGENT_BIN:-agent}" send -; then
  echo "$TOTAL" > "$MARK"
  # The pictures that Claude read in this turn (Read calls on .png .jpg .jpeg .webp .gif that still exist) follow the
  # answer into the chat, at most 3 and each once. `agent send-file` has the last word: it refuses secrets, links to
  # them, empty and oversized files, and sends at most 10 files a minute; its refusals are of no interest here. They
  # are taken from the lines this run marked as done, so a later Stop of the same turn does not send them again.
  # PB_FORWARD_IMAGES=off turns this off.
  case "${PB_FORWARD_IMAGES:-on}" in
    off | OFF | Off | 0 | false | no) ;;
    *)
      printf '%s\n' "$LINES" |
        jq -Rr 'fromjson? | .message.content[]? | objects | select(.type == "tool_use" and .name == "Read") | .input.file_path? | select(type == "string" and test("\\.(png|jpe?g|webp|gif)$"; "i") and (test("[[:cntrl:]]") | not))' 2>/dev/null |
        awk '!seen[$0]++' | head -3 |
        while IFS= read -r IMAGE; do
          [ -f "$IMAGE" ] && "${AGENT_BIN:-agent}" send-file -- "$IMAGE" < /dev/null > /dev/null 2>&1 || true
        done
      ;;
  esac
fi
exit 0
