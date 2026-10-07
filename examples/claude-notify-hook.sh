#!/bin/sh
# Claude Code "Notification" hook: tells the chat that Claude is waiting for you, for example at a permission prompt or a
# question with choices. Without it, a prompt that waits for an answer is invisible on the phone, because the "Stop" hook
# fires only when a turn ends.
#
#   ~/.claude/settings.json:
#     { "hooks": { "Notification": [ { "matcher": "permission_prompt|elicitation_dialog", "hooks": [ { "type": "command", "command": "/path/to/claude-notify-hook.sh" } ] } ] } }
#
# Needs the `agent` binary (on PATH, or set AGENT_BIN) and tmux on PATH. Set PB_STATE_DIR if the agent uses a non-default one.
# The notice has a button for each choice and one for Esc; /key N (the number of your choice), /enter and /esc work too.
#
# The matcher keeps out the notifications that need no answer: Claude Code sends "idle_prompt" a minute after every
# finished turn and "auth_success" when a login completes. `agent prompt` drops those two itself as well, so a settings file
# without the matcher does not fill the chat. Any other type is forwarded, and so is a notification with no type at all
# (older Claude Code versions do not send one), because missing a prompt that waits for you costs more than a spare message.
#
# Claude Code's own text ("Claude needs your permission") does not say what is being asked, and a phone cannot approve what
# it cannot see. `agent prompt` therefore also puts the prompt as it is on the screen of the tmux pane that Claude Code runs
# in (the hook inherits TMUX_PANE) into the notice: its title, the command or the question, and the numbered choices. The
# prompt is not in the transcript yet while it waits, so the screen is the only place to read it from. It is the part of the
# screen after the last full-width rule; it is sent only if it has numbered choices. The text goes to the chat, so values
# after words like token, secret, password, passphrase, api key, authorization and cookie, the credentials of a URL, the
# password flags of some programs, PEM blocks and tokens of well-known shapes are replaced by <hidden>, and a notice that
# has something hidden says "(some values are hidden)". That is best effort, not a guarantee: a secret in a form it does
# not know goes to the chat, and a value it hides may be part of what you are asked to approve. A dialog over 1800
# characters loses its middle (the first lines and the last 500 characters stay, marked "[N characters left out]"), and the
# notice then has no button but Esc: the part you cannot see may be the one that matters, so read it on the laptop (or
# /screen) and answer with /key N.
#
# Each button is tied to the prompt it was made for, by a hash of the dialog. If the prompt has changed by the time you tap
# (an old notice, another command), the agent presses nothing and says so; moving the highlight on the laptop does not
# change the hash. The hash is keyed with a key that `agent prompt` and the agent share through PB_STATE_DIR (the file
# prompt-key, made on first use), so run the hook with the same PB_STATE_DIR as the agent. Without TMUX_PANE, without a
# prompt on the screen, when tmux cannot be asked, or when the key cannot be read, the plain notice goes out, without buttons.
set -eu
exec "${AGENT_BIN:-agent}" prompt
