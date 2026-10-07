#!/bin/sh
# Tells the chat at once when someone attaches to the tmux session that Claude Code runs in, and can disconnect them.
#
#   tmux-attach-alert.sh install      put the hooks on the session (needs the session; run it again to change a setting)
#   tmux-attach-alert.sh uninstall    take them off
#   tmux-attach-alert.sh guard CMD... install the hooks, then run CMD in this process: put it in front of the command that
#                                     starts Claude Code, so that the hooks are back after every restart of the session
#                                     (PB_CLAUDE_COMMAND="sh /path/tmux-attach-alert.sh guard claude --continue")
#
# The hooks are tmux's client-attached and client-session-changed on the one session. Both fire for one attach, so a client
# is reported once, by its pid and start time. When one fires, tmux runs this script without arguments: it lists the clients
# of the session, and for each one it has not seen yet it sends an alert to the chat with `agent send -type alert` (the
# client's pid, terminal, mode and the program it was started from) and, if asked, disconnects it first.
#
# Settings (environment, read when `install` runs and put into the hook's command line, so run `install` again to change):
#   PB_TMUX_SESSION   the session (default claude-dev)
#   PB_STATE_DIR      the agent's state directory, as for the other hooks; the list of clients seen is kept there
#   AGENT_BIN         the agent binary (default: agent on PATH)
#   PB_ATTACH_KICK=1  disconnect a client that attaches. Off by default, because it disconnects you too. Attaching is
#                     allowed for a while by writing an end time (seconds since 1970) to $PB_STATE_DIR/attach-allow-until:
#                       echo $(( $(date +%s) + 600 )) > "$PB_STATE_DIR/attach-allow-until"
#
# What this is and is not. tmux has no access control beyond the permissions of its socket, so any process of the same user
# can attach, type into the session and read the screen, and none of this stops it. It notices a client that attaches. It does
# not see `tmux send-keys` or `capture-pane` from a script, which have no client, and whoever can reach the socket can also
# take the hooks off (`tmux set-hook -u`) before attaching. The disconnect takes a moment, in which the client has the screen.
# A separate account for the session is the protection that holds; this is the alarm.
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
SESSION="${PB_TMUX_SESSION:-claude-dev}"
STATE_DIR="${PB_STATE_DIR:-${XDG_STATE_HOME:-$HOME/.local/state}/petrvs-bridge}"
AGENT="${AGENT_BIN:-agent}"
KICK="${PB_ATTACH_KICK:-0}"
SEEN="$STATE_DIR/attach-seen"
FORMAT="#{client_name}|#{client_pid}|#{client_tty}|#{client_readonly}|#{client_created}|#{client_control_mode}"

# Every target below is "=NAME:": the "=" matches the session of exactly that name (a plain NAME also matches the start of
# another name), and set-hook and show-hooks take the colon, because they expect a pane and refuse a bare "=NAME".

# safe says whether a value may go into the command line of a hook: tmux expands # and %, sh reads quotes and $, and a
# hook would then run what the value says. Only plain path characters pass.
safe() {
  [ -n "$1" ] && ! printf '%s' "$1" | grep -q '[^A-Za-z0-9_./:@+=,~ -]'
}

# remember_current notes the clients that are attached now as seen: they were there before the hooks, or are the owner's.
remember_current() {
  mkdir -p "$STATE_DIR" 2>/dev/null || return 0
  tmux list-clients -t "=$SESSION:" -F '#{client_pid} #{client_created}' 2>/dev/null >>"$SEEN" || true
}

install() {
  for v in "$STATE_DIR" "$AGENT" "$SESSION" "$HERE" "$KICK"; do
    safe "$v" || {
      echo "tmux-attach-alert: not installing: a setting has a character that tmux or sh would read as syntax: $v" >&2
      return 1
    }
  done
  cmd="run-shell -b \"PB_STATE_DIR='$STATE_DIR' AGENT_BIN='$AGENT' PB_TMUX_SESSION='$SESSION' PB_ATTACH_KICK='$KICK' sh '$HERE/tmux-attach-alert.sh' >/dev/null 2>&1\""
  tmux set-hook -t "=$SESSION:" client-attached "$cmd" && tmux set-hook -t "=$SESSION:" client-session-changed "$cmd" || return 1
  remember_current
}

uninstall() {
  tmux set-hook -u -t "=$SESSION:" client-attached && tmux set-hook -u -t "=$SESSION:" client-session-changed
}

case "${1:-}" in
install) install; exit $? ;;
uninstall) uninstall; exit $? ;;
guard)
  shift
  [ $# -gt 0 ] || { echo "usage: tmux-attach-alert.sh guard COMMAND [ARGS...]" >&2; exit 2; }
  install >/dev/null 2>&1 || true # a failure here must not keep Claude Code from starting
  exec "$@"
  ;;
'') ;;
*) echo "usage: tmux-attach-alert.sh install | uninstall | guard COMMAND [ARGS...]" >&2; exit 2 ;;
esac

# Run by the hook from here on.
mkdir -p "$STATE_DIR" 2>/dev/null || exit 0

# One run at a time: the two hooks of one attach start together, and both would find the client new.
LOCK="$STATE_DIR/attach.lock"
find "$LOCK" -maxdepth 0 -mmin +1 -exec rmdir {} \; 2>/dev/null || true # left behind by a run that was killed
HAVE=""
i=0
while [ "$i" -lt 60 ]; do
  if mkdir "$LOCK" 2>/dev/null; then HAVE=1; break; fi
  i=$((i + 1))
  sleep 0.05
done
trap '[ -z "$HAVE" ] || rmdir "$LOCK" 2>/dev/null' EXIT

NOW=$(date +%s)
ALLOW=$(cat "$STATE_DIR/attach-allow-until" 2>/dev/null || true)
case "$ALLOW" in '' | *[!0-9]*) ALLOW=0 ;; esac

tmux list-clients -t "=$SESSION:" -F "$FORMAT" 2>/dev/null |
  while IFS='|' read -r name pid tty ro created ctl; do
    case "$pid$created" in '' | *[!0-9]*) continue ;; esac
    grep -qxF -- "$pid $created" "$SEEN" 2>/dev/null && continue
    echo "$pid $created" >>"$SEEN"

    action="It is still attached."
    if [ "$KICK" = "1" ]; then
      if [ "$NOW" -lt "$ALLOW" ]; then
        until=$(date -r "$ALLOW" +%H:%M 2>/dev/null || date -d "@$ALLOW" +%H:%M 2>/dev/null || echo "$ALLOW")
        action="Not disconnected: attaching is allowed until $until."
      elif tmux detach-client -t "$name" 2>/dev/null; then
        action="It was disconnected."
      else
        action="Disconnecting it failed."
      fi
    fi

    mode="read-write"
    [ "$ro" = "1" ] && mode="read-only"
    [ "$ctl" = "1" ] && mode="$mode, control mode"
    where="pid $pid"
    [ -z "$tty" ] || where="$where, terminal $tty"
    ppid=$(ps -o ppid= -p "$pid" 2>/dev/null | tr -d ' ')
    parent=""
    [ -z "$ppid" ] || parent=$(ps -o user=,comm= -p "$ppid" 2>/dev/null | tr -d '[:cntrl:]' | tr -s ' ' | cut -c1-100)
    from=""
    [ -z "$parent" ] || from=", started from $parent"
    # Markdown, as the chat shows it: the headline in bold, the session and the client in code, the rest in italics.
    printf '%s' "**Someone attached to the tmux session** \`$SESSION\` at $(date +%H:%M:%S)
\`$where, $mode$from\`
_If this was you, ignore this._ $action" |
      "$AGENT" send -type alert - >/dev/null 2>&1 || true
  done

if [ -f "$SEEN" ]; then
  tail -n 100 "$SEEN" >"$SEEN.tmp" 2>/dev/null && mv "$SEEN.tmp" "$SEEN"
fi
exit 0
