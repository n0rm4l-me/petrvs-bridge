#!/bin/sh
# Claude Code "Notification" hook: tells the chat that Claude is waiting for you, for example at a permission prompt.
# Without it, a prompt that waits for an answer is invisible on the phone, because the "Stop" hook fires only when a turn ends.
#
#   ~/.claude/settings.json:
#     { "hooks": { "Notification": [ { "matcher": "permission_prompt|elicitation_dialog", "hooks": [ { "type": "command", "command": "/path/to/claude-notify-hook.sh" } ] } ] } }
#
# Needs jq and the `agent` binary (on PATH, or set AGENT_BIN). Set PB_STATE_DIR if the agent uses a non-default one.
# Answer the prompt from the chat with /key 1 to /key 4 (the numbered choices), /enter or /esc.
#
# The matcher keeps out the notifications that need no answer: Claude Code sends "idle_prompt" a minute after every
# finished turn and "auth_success" when a login completes. This script drops those two itself as well, so a settings file
# without the matcher does not fill the chat. Any other type is forwarded, and so is a notification with no type at all
# (older Claude Code versions do not send one), because missing a prompt that waits for you costs more than a spare message.
set -eu
INPUT=$(cat)
TYPE=$(printf '%s' "$INPUT" | jq -r '.notification_type // empty' 2>/dev/null || true)
case "$TYPE" in
  idle_prompt | auth_success) exit 0 ;;
esac
MSG=$(printf '%s' "$INPUT" | jq -r '.message // empty' 2>/dev/null || true)
[ -n "$MSG" ] || exit 0
printf '%s' "⏳ $MSG. Answer with /key 1–4 (the numbered choices), /enter or /esc." | "${AGENT_BIN:-agent}" send -type notice -
exit 0
