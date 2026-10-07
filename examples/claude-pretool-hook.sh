#!/bin/sh
# Claude Code "PreToolUse" hook: gets what Claude has written so far to the chat before it acts, and before a command that may
# take the machine or the bridge down it also says so and waits for the chat to have it.
#
#   ~/.claude/settings.json (no matcher: it is for every tool):
#     { "hooks": { "PreToolUse": [ { "hooks": [ { "type": "command", "command": "/path/to/claude-pretool-hook.sh" } ] } ] } }
#
# Why: the "Stop" hook sends the answer when the turn ends. A turn can take ten minutes, and all that time the words Claude
# writes between its actions ("I will read the code first", "the build passes, now the tests") stay in the transcript; they
# reach the chat in one lump when the turn is over. And a turn that ends with a shutdown never ends: the machine is gone before
# the answer is written. Before each tool call, this hook
#   1. queues the text of the turn so far, as the Stop hook would send it (claude-stop-hook.sh, which must be in the same
#      directory; what it sends here is not sent again when the turn ends), at most once in PB_PROGRESS_EVERY seconds
#      (default 20; 0 turns this off and leaves only the part below). Text written in between goes with the next call or at
#      the end of the turn, so a burst of tool calls does not become a burst of messages;
#   2. for a Bash command that looks like it will take the machine or the bridge down (shutdown, reboot, log out, sleep,
#      killing the bridge or its tmux session) it does not wait for the interval: it queues the text at once, then a notice
#      that such a command is coming and that what Claude says after it reaches the chat only when the bridge runs again (the
#      command itself is not in the notice: it may hold secrets), and waits until the spool is empty, which is when the hub
#      has had the messages written to the chat (it confirms after the chat accepted them), or PB_FLUSH_WAIT seconds
#      (default 10; 0 does not wait) have passed.
# Then the tool runs. The hook never blocks it and never answers the permission question: whatever the permission mode would
# have done without the hook, it still does.
#
# Needs jq and the `agent` binary (on PATH, or set AGENT_BIN). Set PB_STATE_DIR if the agent uses a non-default one, as for
# the Stop hook: the spool is $PB_STATE_DIR/spool.
#
# Which commands count as dangerous is a guess from the text of the command, not a promise: a shutdown behind a script, a
# variable or an `ssh` goes unseen. PB_FLUSH_PATTERN adds an extended regular expression (case does not matter) for the ones
# you know of. A false alarm costs a notice and, with the agent not running, up to PB_FLUSH_WAIT seconds.
set -eu

LOCK=""
cleanup() {
  [ -z "$LOCK" ] || rmdir "$LOCK" 2>/dev/null || true
  exit 0 # whatever goes wrong here, the tool is not held back by a failure of this hook
}
trap cleanup EXIT

INPUT=$(cat)
TOOL=$(printf '%s' "$INPUT" | jq -r '.tool_name // empty')
TRANSCRIPT=$(printf '%s' "$INPUT" | jq -r '.transcript_path // empty')

# Where a command can start: the beginning of a line, after ; & | ( { or $(, after sudo, exec, nohup and the like with their
# flags ("sudo -u root", "env -i"), and a path before the program's name. A backtick is not one of them: in a Markdown text
# that a command writes into a file it quotes a word, and `shutdown` there is no shutdown.
START='(^|[;&|({]|\$\()[[:space:]]*((sudo|doas|exec|nohup|time|command|env|nice)[[:space:]]+(-u[[:space:]]+[^[:space:]]+[[:space:]]+|-[^[:space:]]+[[:space:]]+)*)*([^[:space:];&|]*/)?'
END='([[:space:]]|$|;|&|\|)'

WHAT=""
look() { # look LABEL REGEX: the first one that fits names the notice
  [ -z "$WHAT" ] || return 0
  if printf '%s\n' "$CMD" | grep -Eiq -e "$2"; then WHAT=$1; fi
}
if [ "$TOOL" = "Bash" ]; then
  CMD=$(printf '%s' "$INPUT" | jq -r '.tool_input.command // empty')
  if [ -n "$CMD" ]; then
    look "power off, restart, log out or sleep" "${START}(shutdown|reboot|halt|poweroff|fastboot)${END}"
    look "power off, restart, log out or sleep" "${START}osascript[[:space:]].*(shut down|restart|log out)"
    look "power off, restart, log out or sleep" "${START}pmset[[:space:]].*sleepnow"
    look "power off, restart, log out or sleep" "${START}(systemctl|loginctl)[[:space:]]+(-[^[:space:]]+[[:space:]]+)*(poweroff|reboot|halt|suspend|hibernate|kexec)${END}"
    look "stop the bridge or its tmux session" "${START}tmux[[:space:]]+(-[^[:space:]]+[[:space:]]+([^-[:space:]][^[:space:]]*[[:space:]]+)?)*kill-(session|server)${END}"
    look "stop the bridge or its tmux session" "${START}(pkill|killall)[[:space:]].*(petrvs|agent|hub|claude|run\\.sh|emulator)"
    look "stop the bridge or its tmux session" "${START}launchctl[[:space:]]+(bootout|unload|stop|remove|kickstart)${END}"
    [ -z "${PB_FLUSH_PATTERN:-}" ] || look "a command you named in PB_FLUSH_PATTERN" "$PB_FLUSH_PATTERN"
  fi
fi

number() { # number VALUE DEFAULT: VALUE if it is a non-negative whole number, else DEFAULT
  case "$1" in '' | *[!0-9]*) printf '%s' "$2" ;; *) printf '%s' "$1" ;; esac
}
EVERY=$(number "${PB_PROGRESS_EVERY:-}" 20)
[ -n "$WHAT" ] || [ "$EVERY" -gt 0 ] || exit 0

HERE=$(cd "$(dirname "$0")" && pwd)
STATE_DIR="${PB_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/petrvs-bridge}"
AGENT="${AGENT_BIN:-agent}"
mkdir -p "$STATE_DIR"

# One run at a time: tool calls that Claude makes together start their hooks together, and two runs would both queue the same
# text. A run that finds the lock taken leaves the text to the one that holds it. A lock left behind by a run that was killed
# is removed after a minute.
find "$STATE_DIR/progress.lock" -maxdepth 0 -mmin +1 -exec rmdir {} \; 2>/dev/null || true
if mkdir "$STATE_DIR/progress.lock" 2>/dev/null; then LOCK="$STATE_DIR/progress.lock"; fi

# Queue the text of the turn so far, and note the time if there was some. The Stop hook moves its mark of how far it has
# read when the text is in the spool; a mark that did not move means nothing was queued, and the interval does not start.
SESSION=""
[ -z "$TRANSCRIPT" ] || SESSION=$(basename "$TRANSCRIPT" .jsonl)
queue_text() {
  [ -n "$SESSION" ] && [ -n "$LOCK" ] || return 0
  BEFORE=$(cat "$STATE_DIR/hook-$SESSION" 2>/dev/null || echo 0)
  printf '%s' "$INPUT" | PB_EARLY=1 sh "$HERE/claude-stop-hook.sh" >/dev/null 2>&1 || true
  AFTER=$(cat "$STATE_DIR/hook-$SESSION" 2>/dev/null || echo 0)
  if [ "$BEFORE" != "$AFTER" ]; then date +%s > "$STATE_DIR/progress-$SESSION"; fi
}

if [ -z "$WHAT" ]; then
  # An ordinary call: the text goes if the interval since the last time has passed.
  [ -n "$SESSION" ] && [ -n "$LOCK" ] || exit 0
  NOW=$(date +%s)
  LAST=$(number "$(cat "$STATE_DIR/progress-$SESSION" 2>/dev/null || true)" 0)
  [ "$LAST" -le "$NOW" ] || LAST=0 # a clock that went back
  [ $((NOW - LAST)) -ge "$EVERY" ] || exit 0
  queue_text
  exit 0
fi

queue_text
if printf '%s' "⚠️ Claude Code is about to run a command that may stop this machine or the bridge ($WHAT). What it says after that reaches this chat only when the bridge runs again." |
  "$AGENT" send -type notice - >/dev/null 2>&1; then
  WAIT=$(number "${PB_FLUSH_WAIT:-}" 10)
  # Five checks a second. The hub confirms a message after the chat has it, and the agent then removes its file.
  LEFT=$((WAIT * 5))
  while [ "$LEFT" -gt 0 ] && [ -n "$(find "$STATE_DIR/spool" -maxdepth 1 -name '*.json' 2>/dev/null | head -n 1)" ]; do
    sleep 0.2
    LEFT=$((LEFT - 1))
  done
fi
exit 0
