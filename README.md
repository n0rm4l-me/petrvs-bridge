# petrvs-bridge

Control an AI coding assistant that runs in a terminal from your phone, through a chat app, with a cloud message bus in
the middle and end-to-end encryption between the two ends.

The name is a nod to Saint Peter (Latin *Petrvs*), the keeper of the keys: the hub and each agent pin one another's keys,
and nothing else gets through the gate.

```
phone ── chat service ──> HUB (a small service in the cloud) <── Pub/Sub, encrypted ──> AGENT (next to the tmux session)
                                                                                         │
                                                                                         └─ types into tmux, sends replies back
```

* The **agent** runs on the machine with the terminal. It opens **no listening port** and connects to **nothing but the
  message bus**. It does not know where the hub runs; it knows a project, a topic and a subscription.
* The **hub** talks to the chat service (Telegram) and holds one encrypted session per agent. It can run anywhere that
  can reach Telegram and Pub/Sub, for example a container in Kubernetes.
* Everything between hub and agent is **sealed and signed**: an authenticated handshake (pinned keys, X25519 and
  ML-KEM-768), then a double ratchet with a fresh key for every message. The bus operator sees ciphertext of uniform
  size. A forged, replayed, delayed or reordered message cannot make the agent type anything, and cannot break a live
  session.
* The chat leg is **not** end-to-end encrypted (bot chats never are): the hub sees the plain text. Design accordingly, see
  [docs/SECURITY.md](docs/SECURITY.md).

Besides text and keys, the bridge shows the prompt that Claude Code waits on (the command, the numbered choices) with a
button for each choice; sends files and screenshots from the machine to the chat and takes photos and documents from the
chat to the machine; shows the state of each machine (battery, disk, uptime, what runs in tmux); starts Claude Code again
when it stops; and says so when a request has had no reply for too long.

Status: working and tested (unit tests, fuzzing, end-to-end tests on the Pub/Sub emulator, and a smoke test with the real
binaries, a real tmux and a fake chat server). It is young, the protocol is custom, and it has **not had an independent
security audit**. Read the limitations before trusting it with anything that matters.

## Quick start on one machine (Pub/Sub emulator)

You need Go 1.26.6 or newer, tmux, and for the emulator `gcloud` with `gcloud components install pubsub-emulator` (and Java). Go 1.21 and later fetch the toolchain named in `go.mod` by themselves when theirs is older, unless `GOTOOLCHAIN=local` is set: the official `golang` Docker images set it, so a build there with an older image stops with `go.mod requires go >= 1.26.6` (the hub's Dockerfile sets `GOTOOLCHAIN=auto` for that reason).

```sh
make build                                   # bin/hub bin/agent bin/bridgekeys

# 1. keys: each side creates its own identity and shares only the .pub.json
bin/bridgekeys gen -name hub    -out keys
bin/bridgekeys gen -name laptop -out keys
mkdir -p agents && cp keys/laptop.pub.json agents/      # the hub pins the agents it accepts
# Compare the printed fingerprints over a second channel whenever you copy a public bundle.

# 2. a bus
gcloud beta emulators pubsub start --host-port=127.0.0.1:8085 --project=demo &
export PUBSUB_EMULATOR_HOST=127.0.0.1:8085 PB_PROJECT=demo PB_CREATE_RESOURCES=true

# 3. the hub (the bot token comes from the environment, never from a flag)
export TELEGRAM_BOT_TOKEN=...        # from @BotFather
PB_IDENTITY=keys/hub.identity.json PB_AGENTS_DIR=agents PB_CHAT_ID=<your chat id> bin/hub &

# 4. a tmux session with the assistant, and the agent next to it
tmux new-session -d -s claude
PB_NAME=laptop PB_IDENTITY=keys/laptop.identity.json PB_HUB_PUB=keys/hub.pub.json PB_TMUX_SESSION=claude bin/agent
```

Write to the bot. The text is typed into the `claude` session; replies that a hook queues with `agent send` come back to
the chat. See [examples/claude-stop-hook.sh](examples/claude-stop-hook.sh) for a Claude Code hook that does that.

## Chat commands

| Message | Effect |
|---|---|
| any text | typed into the active agent's session, then Enter |
| `@name text` | the same, for another agent |
| `@name /enter`, `@name /key 2`, `@name /alarm on` | a key press or an alarm command on that agent. Any other slash text after `@name`, such as `@name /compact`, is typed as it is |
| answer (swipe-reply) to a message of an agent | your text, or a key command, goes to the agent that wrote that message, whatever is active; the hub notes the destination when it differs from the active agent; if the hub no longer remembers that message and there are several agents, nothing is sent and it asks for `@name` or `/agent` |
| `/agents` or `/status` | one card per agent (see below): online or offline, how long ago it was heard, battery, uptime, free disk, whether the tmux session is there and what runs in it, the permission mode, the queue, lock and alarm. What an agent did not report is left out; an offline agent shows its last known facts, marked as old |
| `/agent NAME` | choose the active agent. `/agent` alone, `/agents` and `/status` show buttons, one per agent, the active one marked with ●; a tap chooses |
| `/enter` `/esc` `/up` `/down` `/tab`, `/key N` | press a key (`N` is 1 to 9) |
| `/key btab` | press Shift+Tab, which cycles Claude Code's permission mode (ask, accept edits, plan, auto ...). It is deliberately not a one-tap command and not in the bot's menu. It cycles, so it cannot pick a mode, but the agent reports the mode it reached. See the caveat in docs/SECURITY.md. |
| `/stop` | interrupt what Claude Code is doing (presses Escape) |
| `/clear`, `/compact [text]`, `/resume [text]` | type Claude Code's own command. After `/resume` the agent sends the screen, so that you can pick a session with the buttons. Not in the bot's menu: a tap by mistake would clear the conversation |
| `/sessions` | asks Claude Code to list its sessions in a short table |
| `/screen` | the bottom of the screen, or the prompt that waits for you, with buttons ↑ ↓ ⏎ and Esc. Secrets are hidden as far as that can be done; a locked machine refuses it |
| `/continue [NAME]` | start Claude Code in the agent's tmux session if it is not running there (session gone: it is created; a shell in the foreground: `cd DIR && exec COMMAND` is typed); otherwise it says that Claude Code is already running. Needs `PB_WORKDIR`. Not in the bot's menu, not counted against the auto-heal tries, refused on a locked machine |
| a photo or a document (a caption is fine) | saved in the agent's inbox and announced to the session with a line like `[photo from the chat, saved at /path (jpg, 48 KB)] caption`. Routed like text (swipe-reply, else the active agent; a caption cannot name an agent). At most `PB_MAX_FILE_BYTES`, two at a time; refused when the agent is offline or locked |

`/stop`, `/clear`, `/compact`, `/resume`, `/sessions`, `/screen` and `/continue` also take `@name`, a swipe-reply, or the
active agent, like the keys.
| `/alarm on` | arm the alarm (see below) |
| `/alarm off [code]` | disarm; the code is needed if the agent has an alarm secret |
| `/alarm` or `/alarm status` | state and which sensors are working |
| `/lockdown` | the hub stops relaying and tells agents to refuse text and keys until unlocked **on their own machine**. Always obeyed, whatever the message's age |
| `/unlock` | the hub relays again (agents stay locked) |
| `/help` or `/start` | the list of commands |

Only the chat id and user id you configure are obeyed; everyone else is ignored and counted.

### Prompts, files and the state of the machine

**Buttons under a prompt.** When Claude Code stops to ask (a permission, a question with choices), the Notification hook runs
`agent prompt`, which reads the prompt from the screen of the tmux pane and sends it to the chat with one button per choice
and an Esc button. A tap presses the key only if that prompt is still on the screen: each button carries a hash of the
dialog, so a button of an old notice cannot approve a new command, even one that differs only in the command, and moving
the highlight on the laptop does not matter. The hash is keyed (HMAC with a key in the agent's state directory, created on
first use, `prompt-key`), so run the hook with the same `PB_STATE_DIR` as the agent; if the key cannot be read, the hook
sends the plain notice without buttons. The choices are the last numbered list in the dialog, so a numbered list inside
the command does not pass for them. A tapped message loses its buttons, so a prompt is answered once, and buttons work for
15 minutes after the message was sent (after that, or after a hub restart, `/screen` gives new ones). A tap that could not
be sent is answered "Not sent" and the buttons stay. The navigation buttons of `/screen` have no hash; the agent refuses
their Enter and digits while a prompt is on the screen. `/key N`, `/enter` and `/esc` still work.

A notice that says "(some values are hidden)" has had secrets hidden. A command too long for the notice (over 1800
characters) loses its middle and gets only an Esc button and the line "The command is too long to show here": read it on
the laptop or with `/screen`, then answer with `/key N`.

**Files from the machine.** `agent send-file [-caption TEXT] PATH` (what Claude runs when you ask it to send you something)
queues a file for the hub to upload to the chat: a photo for png, jpg and webp up to 10 MiB, otherwise a document. The Stop
hook also sends the pictures Claude read during the turn. The agent refuses secrets before anything leaves the machine:
by name (`.env`, `id_rsa`, `*credential*`; names are compared without regard to case and Unicode variants, so `.AWS` and
`.awſ` are `.aws`), by place (`~/.ssh`, `~/.aws`, the agent's own state), by content (private keys, tokens) and by identity
(the same file as the agent's key, the alarm secret or a file in its state directory, under any name, link or hard link).
Files in the inbox are not sent back, so the Stop hook does not return your own photo. The file is opened first and judged
by where the open descriptor really is, so a directory swapped for a link during the call cannot redirect the read (on a
system where that path cannot be asked for, which is neither Linux nor macOS, the check by path stays). It sends at most
10 files a minute, and holds files back while the machine is locked. The list is a guard against accidents, not against
someone who renames a file.

**Files to the machine.** The hub fetches the photo or document from Telegram (bots can download at most 20 MB), sends it
part by part to the agent in batches of up to 16 unanswered parts, at most one batch every 250 ms (the receiver's ratchet
skips at most 50 message keys and takes a limited number of new keys a second, so a burst would be refused; a file of
20 MiB takes a few seconds, 45 MiB about six), and then sends the command that makes the agent put the file together, check its size and
SHA-256, save it in the inbox (`PB_INBOX_DIR`, mode 0700, files 0600, never overwriting) and type one line into the session
that says where it is. Names are reduced to harmless characters; nothing from a file is ever executed. The inbox keeps the
newest 200 files for at most 14 days. Text and keys that you send while a file is still on its way wait behind it (at most
32 per agent) and are typed after the line about the file, in the order of the chat; `/lockdown` discards them. If a
session is re-established while commands wait in the agent's queue, the chat gets one "dropped" line for each; send them
again. An agent can queue up to 16 files at once.

**The state of the machine.** Every heartbeat carries a few facts (battery, uptime, disk, the tmux session and what runs in
it, the permission mode, the queue). `/status` turns them into a card per agent:

```
laptop — online, heard 12 s ago
🔋 83% charging · ⏱ up 3d4h · 💾 212G free of 460G
🖥 tmux ok · claude · auto mode · queue 0
🔓 unlocked · 🚨 alarm off
```

**Auto-heal.** With `PB_HEAL=true` (and `PB_WORKDIR`) the agent asks tmux every few seconds what runs in the pane. If the
session is gone or a shell is in the foreground for the whole grace period (`PB_HEAL_GRACE`), it starts Claude Code again
(creates the session, or types `cd DIR && exec COMMAND` into the shell), tells the chat, and after `PB_HEAL_TRIES` starts
within `PB_HEAL_WINDOW` gives up with one message until Claude Code runs again. It does nothing on a locked machine, while
tmux gives no answer, when you restart Claude Code by hand within the grace, or when the pane looks like Claude Code's own
screen. The command and the directory come from the agent's settings only, never from the chat. At a shell the agent types
the start command only into an idle prompt: the last line of the screen must be a prompt with nothing typed after it and
must have stayed the same for the whole grace period. If you have begun a line, or the pane is scrolled back, the grace
period starts again and nothing is typed. `/continue` makes the same check and says so when it refuses. A shell prompt that
ends in none of `% $ # > ❯ ➜ » → λ ▶` is never auto-healed. After a start by auto-heal or `/continue`, auto-heal waits one
grace period before it can start again.

**A request without a reply.** A text from the chat starts a clock and the assistant's reply stops it. After
`PB_STUCK_AFTER` without a reply the chat gets a notice with 📺 Screen and ✖ Stop buttons, at most 3 times. It is silent
while a prompt waits for your answer, and Claude Code's own commands such as `/clear` start no clock. The clock starts when
the text is handed to tmux, before it is typed, and is dropped if typing fails. A text that starts with `!` (Claude
Code's bash mode) starts no clock. A tap on ✖ Esc or `/stop` ends the request even while a prompt waited. No notice is
queued while a reply is waiting in the spool.

**Text is not typed into a shell or into a dialog.** The agent looks at the active pane of the session's window (with a
split window, the pane that receives the keys). If Claude Code has exited and a shell is in the foreground, a text from
the chat is refused ("a shell, not Claude Code ...") instead of being run as a command; the screen must show Claude Code's
interface at its end: what Claude Code left above a shell prompt after exiting does not count. If a dialog with numbered
choices is on the screen, no text is typed either, because the Enter after it would pick the highlighted choice: the chat
is told to answer with the buttons or `/key N` and to send the text again (so a free-text answer such as "Type something
else" cannot be sent from the chat while the list is on screen). The same holds for the line that tells the session about
a file (the file stays saved). A pane that is scrolled back with the mouse (copy mode) is left first, so text and keys
reach Claude Code. `/continue` starts Claude Code.

The hub publishes the commands as the bot's menu (what Telegram shows when you type `/`) each time it starts, and marks every
message of yours with a reaction as soon as it has read it (`PB_RECEIVED_REACTION`, 👀 by default), and replaces it with
another (`PB_DELIVERED_REACTION`, 👍 by default) when the agent reports that it has typed the text into the session, so you
can see on the phone that it arrived and that it was delivered. A command the agent refused or dropped keeps 👀, and the hub
says why in the chat. All of this is a courtesy: a failure is logged and never holds up a command.

A command older than its time to live is **dropped and reported**, never typed: text lives 10 minutes, keys and alarm
commands one minute, counted from when you sent the message (so a backlog that Telegram delivers after an outage of the
hub is ignored, and the hub says so). The agent checks again just before it types. A machine that was asleep overnight
will not press buttons on a prompt you no longer see. The hub also refuses to queue a command for an agent it has not
heard from recently.

Commands are typed **in the order you sent them**, one at a time, even though the bus does not keep order. If one goes
missing, the ones after it wait `PB_REORDER_WINDOW` (2 s), then the missing one is reported as lost and the rest go ahead.

## Several machines

Each machine runs its own agent with its own keys and a name. Where does your text go?

* With **one** agent, to it. With **several**, the hub does not guess: until you choose one (`/agent NAME`, or a tap on a
  button under `/agent` or `/agents`), plain text, key commands and `/alarm` are not sent, and the hub asks you to choose.
  A restart of the hub (an upgrade in Kubernetes, for example) forgets the choice, so a "yes" meant for one machine cannot
  land on another. `PB_DEFAULT_AGENT` names an agent that is used until you choose, if you prefer that.
* `@name text` writes to one agent without changing the choice; `@name /enter` presses a key on it.
* Answering a message of an agent (swipe-reply in Telegram) sends your answer to **that** agent, so a question from a
  machine that is not the chosen one is answered where it was asked. The hub remembers the last 512 messages for this; if it has forgotten the one you answered (after a restart, say) and there are several agents, it sends nothing and asks for `@name` or `/agent` instead of guessing.
* Replies, notices and alarms carry `[name]` when there is more than one agent. `/lockdown` applies to all of them.

## Alarm mode

`/alarm on` arms the machine. While it is armed, any sign that someone touched it is reported to the chat at once:
keyboard, mouse or trackpad (input idle time resets), lid open or closed, the power cable plugged or unplugged, a USB
device attached or removed. The person who armed it is not reported: a fresh baseline is taken when it arms. Commands the
agent types itself go through tmux and do not count as input.

* Reports are grouped: the first touch is reported immediately, then at most one report a minute per sensor with a count
  of what was held back.
* Reports go through the same spool with receipts as replies, so they survive a hub restart, a chat outage or a sleeping
  laptop; a late report still carries the time it happened.
* If the agent goes offline while armed, the offline notice says so.
* Set `PB_ALARM_SECRET_FILE` to a secret made with `bridgekeys totp-secret`, and disarming needs a current one-time code
  from an authenticator app (`/alarm off 123456`): someone who takes over your chat cannot silence the alarm. A used code
  does not work twice. A locked machine (`/lockdown`) refuses to be disarmed from the chat.
* Sensors: macOS (`ioreg`, `pmset`) and Linux (`xprintidle`, `lsusb`). The alarm reports which sensors it cannot read.

## Configuration reference

Every setting is an environment variable and most are also flags (`-name value`). Secrets are read from the environment or
from a file that must not be readable by other users, never from a flag. A missing or invalid setting makes the program
list every problem at once and exit. `-h` or `--help` prints the flags with their defaults and exits with status 0 (an
unknown flag prints them too, and exits with 2).

### Hub (`bin/hub`)

| Variable | Flag | Default | Meaning |
|---|---|---|---|
| `PB_IDENTITY` | `-identity` | required | Path of the hub's private identity file (mode 0600) |
| `PB_AGENTS_DIR` | `-agents-dir` | required | Directory with one `NAME.pub.json` per agent that may connect. Nobody else can |
| `TELEGRAM_BOT_TOKEN` | none | required (or the next) | Bot token |
| `PB_TELEGRAM_TOKEN_FILE` | none | | File holding the bot token (mode 0600). Used if the variable above is unset |
| `PB_CHAT_ID` | `-chat-id` | required | The only chat that is obeyed |
| `PB_USER_ID` | `-user-id` | the chat id | The only user that is obeyed (in a private chat it equals the chat id) |
| `PB_PROJECT` | `-project` | required unless the emulator is used; then `emulator` | Pub/Sub project id (`GOOGLE_CLOUD_PROJECT` is also read). With `PUBSUB_EMULATOR_HOST` set and no project, hub and agent both use `emulator`; set the same one on both if you set one |
| `PB_PREFIX` | `-prefix` | `pb` | Prefix of topic and subscription names |
| `PB_CREATE_RESOURCES` | `-create-resources` | `false` | Create topics and subscriptions. For development and the emulator only; create them with infrastructure code in production |
| `PB_TELEGRAM_API_BASE` | `-telegram-api` | `https://api.telegram.org` | Bot API address (a local Bot API server, or a test double) |
| `PB_OFFLINE_AFTER` | `-offline-after` | `3m` | Silence after which an agent is called offline |
| `PB_TEXT_TTL` | `-text-ttl` | `10m` | How long a text command stays valid |
| `PB_KEY_TTL` | `-key-ttl` | `1m` | How long a key press or an alarm command stays valid |
| `PB_POLL_TIMEOUT` | `-poll-timeout` | `25s` | Telegram long-poll timeout |
| `PB_MAX_REPLY_BYTES` | `-max-reply-bytes` | `20000` | Longest reply shown in the chat (longer ones are cut, with a note) |
| `PB_MAX_FILE_BYTES` | `-max-file-bytes` | `20971520` (20 MiB) | Largest file in either direction, at most 50 MiB (Telegram lets a bot download 20 MB) |
| `PB_DEFAULT_AGENT` | `-default-agent` | unset | Agent that gets plain text until you choose one. Unset with several agents means the hub asks first; with one agent that one is used |
| `PB_DELIVERED_REACTION` | `-delivered-reaction` | `👍` | Emoji that replaces the first one when the agent has typed the text (or pressed the key) in its session; `off` leaves the first one. Same rules as above |
| `PB_RECEIVED_REACTION` | `-received-reaction` | `👀` | Emoji the hub puts on each of your messages once it has read it; `off` turns it off. Telegram accepts only a fixed set of reaction emoji (see `ReactionTypeEmoji` in the Bot API documentation, for example 👀 👍 🔥 🎉 🫡 🤝 ⚡); if it refuses yours the hub says so once in its log and stops marking |
| `PB_LOCK_FILE` | `-lock-file` | unset | File that keeps the hub's lockdown across restarts of the process. Unset, a restart forgets a lockdown. In Kubernetes the chart puts it on an `emptyDir`, which is forgotten when the pod is replaced (see Limitations) |
| `PB_ALLOW_GROUP_READ` | `-allow-group-read` | `false` | Also accept private files that are group-readable (0640), for a Kubernetes secret volume with `fsGroup`. Never world-readable |
| `PB_LOG_LEVEL` | `-log-level` | `info` | `debug`, `info`, `warn`, `error`. Logs never contain message text or tokens. Both programs write JSON lines; the hub names the fields `severity` (`DEBUG`, `INFO`, `WARNING`, `ERROR`) and `message`, which is what Cloud Logging on GKE reads, and the agent `level` and `msg` |
| `PUBSUB_EMULATOR_HOST` | none | unset | Use the emulator instead of the real service |
| `GOOGLE_APPLICATION_CREDENTIALS` | none | | Standard Google credentials (or workload identity, or the metadata server) |

### Agent (`bin/agent`)

| Variable | Flag | Default | Meaning |
|---|---|---|---|
| `PB_NAME` | `-name` | required | This agent's name; must equal the name in its identity file |
| `PB_IDENTITY` | `-identity` | required | Path of the agent's private identity file (0600) |
| `PB_HUB_PUB` | `-hub-pub` | required | The hub's public bundle, pinned at enrolment |
| `PB_PROJECT`, `PB_PREFIX`, `PB_CREATE_RESOURCES`, `PB_LOG_LEVEL` | | as for the hub | Same meaning |
| `PB_TMUX_SESSION` | `-tmux-session` | `claude` | The tmux session that receives the text |
| `PB_STATE_DIR` | `-state-dir` | `$XDG_STATE_HOME/petrvs-bridge` or `~/.local/state/petrvs-bridge` | Holds `spool/`, `locked`, `alarm-armed` and `alarm-code-state` (used one-time codes). Created with mode 0700 |
| `PB_ALARM` | `-alarm` | `true` | Offer the alarm mode |
| `PB_ALARM_SECRET_FILE` | none | unset | Base32 secret (0600). If set, `/alarm off` needs a one-time code |
| `PB_ALARM_COOLDOWN` | `-alarm-cooldown` | `1m` | At most one report per sensor in this time |
| `PB_ALARM_POLL` | `-alarm-poll` | `1s` | How often the sensors are read |
| `PB_HEARTBEAT` | `-heartbeat` | `30s` | Heartbeat interval |
| `PB_HANDSHAKE_RETRY` | `-handshake-retry` | `10s` | How often a hello is repeated until answered |
| `PB_SILENCE_LIMIT` | `-silence-limit` | 3 x heartbeat | No answer from the hub for this long means a new handshake |
| `PB_RECEIPT_TIMEOUT` | `-receipt-timeout` | `30s` | A queued reply is sent again if the hub has not confirmed it in this time |
| `PB_REORDER_WINDOW` | `-reorder-window` | `2s` | How long a command waits for an earlier one that has not arrived before that one is reported lost |
| `PB_WORKDIR` | `-workdir` | unset | The directory Claude Code runs in; auto-heal and `/continue` start it there, and the inbox is below it |
| `PB_INBOX_DIR` | `-inbox-dir` | `.petrvs-inbox` in the workdir, else `inbox` in the state directory | Where files from the chat are saved. A new inbox directory gets a `.gitignore` containing `*`, so the assistant's `git add -A` does not commit chat files; one that already exists is left as it is, add the ignore line yourself |
| `PB_MAX_FILE_BYTES` | `-max-file-bytes` | `20971520` (20 MiB) | Largest file in either direction, at most 50 MiB |
| `PB_HEAL` | `-heal` | `false` | Start Claude Code again when it has stopped or its tmux session is gone (needs `PB_WORKDIR`) |
| `PB_CLAUDE_COMMAND` | `-claude-command` | `claude --continue` | What auto-heal and `/continue` run to start Claude Code, one line |
| `PB_HEAL_GRACE` | `-heal-grace` | `20s` | Claude Code must have been gone this long before auto-heal acts |
| `PB_HEAL_TRIES`, `PB_HEAL_WINDOW` | `-heal-tries`, `-heal-window` | `3`, `10m` | Starts allowed within the window before auto-heal gives up |
| `PB_STUCK_AFTER` | `-stuck-after` | `10m` | Tell the chat when a request has had no reply for this long; `0` or `off` turns it off |
| `PB_ALLOW_GROUP_READ` | `-allow-group-read` | `false` | As for the hub |
| `GOOGLE_APPLICATION_CREDENTIALS`, `PUBSUB_EMULATOR_HOST` | | | As for the hub |

Other agent commands: `agent send [-type reply|notice|alert] [TEXT | -]` queues a message for the hub (reads standard
input for `-`); it only needs `PB_STATE_DIR` (and exits 1 when the body is too large once escaped for storage).
`agent send-file [-caption TEXT] PATH` queues a file for the hub to upload to the chat; it needs only `PB_STATE_DIR` and
reads `PB_MAX_FILE_BYTES`. It exits 0 when queued, 1 when refused (not a regular file, empty or too large, a secret, a file
in the inbox, more than 10 files a minute, the queue is full) and 2 on a usage error. `agent prompt` is what the Notification hook runs: it
reads the hook's JSON on standard input and queues a notice with the prompt from the screen and its buttons; it needs
`PB_STATE_DIR` and tmux on the `PATH`. `agent unlock`
lifts a lockdown, and is the only way to do it: it prints the path of the lock file it removed, and exits 1 with "no lock
file at PATH" when there was none (a different `PB_STATE_DIR` than the agent's, or a lock held in memory only).

### Keys and secrets (`bin/bridgekeys`)

| Command | Does |
|---|---|
| `gen -name NAME [-out DIR]` | Creates `NAME.identity.json` (private, 0600, refuses to overwrite) and `NAME.pub.json` (share it) and prints the fingerprint |
| `fingerprint FILE` | Prints the fingerprint of a `.pub.json` or `.identity.json` |
| `totp-secret -out FILE` | Creates a one-time-code secret (0600) and prints the `otpauth://` link for an authenticator app |
| `totp-code FILE` | Prints the current code (for testing) |

## Message bus resources

For prefix `P` and each agent `A`:

| Resource | Name | Who |
|---|---|---|
| Topic | `P-to-hub` | agents publish, hub subscribes |
| Subscription | `P-hub` | the hub pulls |
| Topic | `P-to-agent-A` | the hub publishes to it |
| Subscription | `P-agent-A` | agent `A` pulls |

Suggested properties: pull subscriptions, acknowledgement deadline 30 s, **message retention 1 h** (commands expire anyway),
and **`--expiration-period=never`**: by default a subscription that nobody pulls from for 31 days is deleted, which would
silently cut off a laptop that stays closed for a month.

Least-privilege access, one service account per party (shown with `gcloud`):

```sh
# the hub: publish to each agent topic, pull its own subscription
gcloud pubsub topics add-iam-policy-binding P-to-agent-A --member=serviceAccount:HUB_SA --role=roles/pubsub.publisher
gcloud pubsub subscriptions add-iam-policy-binding P-hub --member=serviceAccount:HUB_SA --role=roles/pubsub.subscriber
# agent A: publish to the hub topic, pull its own subscription
gcloud pubsub topics add-iam-policy-binding P-to-hub --member=serviceAccount:AGENT_A_SA --role=roles/pubsub.publisher
gcloud pubsub subscriptions add-iam-policy-binding P-agent-A --member=serviceAccount:AGENT_A_SA --role=roles/pubsub.subscriber
```

The agent uses gRPC. If your network inspects TLS, the machine's trust store must contain the inspecting proxy's root.

## Hooks

[examples/claude-stop-hook.sh](examples/claude-stop-hook.sh) is a Claude Code `Stop` hook: it takes what the assistant
answered since your last message and queues it with `agent send`. The spool keeps the file until the hub confirms it, so
an answer is not lost if the network or the hub is down when it is written. The hook, like any caller of `agent send`,
runs outside the agent, so it must use the same state directory: if the agent has a non-default `PB_STATE_DIR`, export
that value where the hook runs too (the service examples set it explicitly), or the queued answers are never picked up.

The Stop hook also sends the pictures that Claude read during the turn (`Read` of .png, .jpg, .jpeg, .webp or .gif files that
still exist; at most 3 per turn, each once), right after the answer, with `agent send-file`. `PB_FORWARD_IMAGES=off` turns
that off.

[examples/claude-notify-hook.sh](examples/claude-notify-hook.sh) is the `Notification` hook. It runs `agent prompt`, which
reads the prompt from the screen of the tmux pane that Claude Code runs in (`TMUX_PANE`, which Claude Code inherits from
tmux) and sends it to the chat with one button per choice and an Esc button, so that you can answer from the phone knowing
what you answer. Claude Code's own notification text does not say what is asked, and it does not write the pending tool
call to the transcript until you have answered, so the screen is the only place to read the prompt from. It needs Claude
Code to run in tmux, `tmux` on the hook's `PATH` and the agent's `PB_STATE_DIR`; it does not need jq. Give it the matcher
`permission_prompt|elicitation_dialog` in the settings; it drops `idle_prompt` and `auth_success` on its own. The text goes to
the chat, so values that follow words like `token`, `password`, `secret`, `api key`, `authorization` or `bearer`, and the
password in a URL, are replaced by `<hidden>` (so are the password flags of `curl`, `mysql`, `sshpass`, `login` and
`openssl`, PEM blocks and tokens of well-known shapes); a hidden value ends at white space or shell syntax, so the command
after it stays visible. That is best effort, not a guarantee. Without a prompt on the screen, or if the hook cannot read the
agent's `prompt-key`, the plain notice goes out, without buttons.

To keep the agent running, [examples/systemd](examples/systemd/petrvs-bridge-agent.service) (Linux) and
[examples/launchd](examples/launchd/com.example.petrvs-bridge-agent.plist) (macOS) are starting points. Two things in them
matter: the service must see the same tmux socket as your session, so no `PrivateTmp=` in systemd (tmux keeps its socket in
`/tmp`, or in `TMUX_TMPDIR` if you set it, and the agent needs the same value), and launchd needs a `PATH` that includes the
directory of your tmux, as its default does not have Homebrew's. Without them the agent still connects and sends
heartbeats while every tmux call fails. On Linux the unit sets `KillMode=process`: tmux started by auto-heal or `/continue`
lives in the unit's cgroup and would otherwise be killed with the agent on a restart, a stop or a crash. In both examples
the `PATH` must reach `claude` and `node` (or `PB_CLAUDE_COMMAND` must be an absolute path), because after a reboot the tmux
server that auto-heal starts inherits the agent's environment.

## Development

```sh
make test              # unit tests and the in-memory end-to-end tests, with the race detector
make test-emulator     # everything, including the Pub/Sub emulator tests and the real-binary smoke test
make fuzz              # 30 seconds of fuzzing of the ratchet's decrypt
make cover
make vuln            # govulncheck: known vulnerabilities in the code paths this program uses
```

`scripts/with-emulator.sh CMD` runs a command with an emulator available (it starts one with gcloud if
`PUBSUB_EMULATOR_HOST` is unset, on port 18085 or `PB_EMULATOR_PORT`, and stops only that one). If something else already
listens on that port it refuses to run instead of using or stopping it. Tests that need the emulator are skipped without it.

Layout: `internal/ratchet` (the double ratchet), `internal/handshake`, `internal/identity`, `internal/wire` (framing and
payloads), `internal/transport` (Pub/Sub and an in-memory bus with attack helpers), `internal/hubsvc`,
`internal/agentsvc`, `internal/alarm`, `internal/approval` (one-time codes), `internal/spool`, `internal/tmuxctl`,
`internal/telegram`, `internal/config`, `internal/testrig` (the end-to-end test rig), `internal/e2e`, `internal/smoke`.

## Limitations

* **Not audited.** The protocol follows the Signal double ratchet, but the code is new. Get an independent review before
  relying on it.
* The hub sees plain text (the chat does). Someone who controls the hub while it runs can read and relay. It cannot
  forge the agent's one-time codes or unlock a locked machine.
* Ratchet and hub state live in memory. A restart means a new handshake (automatic, a few seconds); a command in flight at
  that moment is lost, a queued reply is not.
* Post-quantum protection covers the handshake (ML-KEM-768 is mixed into the session key, so recorded traffic stays safe).
  The ratchet's later key steps use X25519 only, so there is no post-quantum healing after a state theft. A sparse
  post-quantum ratchet is not implemented.
* The agent trusts its own user. Another process of the same user can read the agent's keys, remove the lock file and
  type into the tmux session; file modes keep other users out, nothing more. Without `PB_ALARM_SECRET_FILE` anyone who
  controls the chat can also disarm the alarm, and the agent says so when it is armed.
* The one-time-code gate (`internal/approval`) is used for disarming the alarm. It is not yet wired to an assistant's
  per-tool approval hook.
* One hub replica only (two hubs would fight over one bot).
* **The hub's lockdown does not survive a new pod.** In Kubernetes the lock file is on an `emptyDir`: it survives a
  restart of the container but not a replacement of the pod, which a `helm upgrade` that changes the pod (a new image, a
  changed agent list), a node drain and an eviction cause, and with the chart's `Recreate` strategy the new hub then
  always starts unlocked, without saying so. An agent that was offline when `/lockdown` was sent never heard it and is
  relayed to again (agents that did hear it stay locked on their machine). Run as a plain binary without `PB_LOCK_FILE`,
  the hub forgets a lockdown on every restart. Send `/lockdown` again after such an event; to keep it, put the file on a
  small PersistentVolumeClaim ([docs/DEPLOY.md](docs/DEPLOY.md)).
* **Commands wait behind a file.** While a photo or document is on its way to the machine, the text and keys you send
  wait behind it (seconds; up to 32 per agent), so a `/stop` or `/esc` typed right after a photo is also delayed.
* **The inbox is in the project.** The default `.petrvs-inbox` is below `PB_WORKDIR`. A new one ignores itself in git, one
  that existed before does not.
* **Hidden values are best effort.** A keyword followed by a space hides the next word (`tokenizer and` becomes
  `tokenizer <hidden>`), and a secret that contains `$` is hidden only up to the `$`. Filesystems that fold case fully
  (such as `ß` and `ss`) are not covered by the name check of `agent send-file`.
* **Pane state is read from tmux, which is not an interface.** Auto-heal and the shell guard rely on the name of the
  foreground program and on how Claude Code draws its screen; they fail toward doing nothing.
* `examples/` and `deploy/` are provided as starting points; the Kubernetes and systemd files have not been run.
* A flood of forged packets with new keys is cut off cheaply, but while the receiver's budget of new-key agreements
  (20 burst, 5 a second) is empty a real message that needs a new key is refused until it refills. The session is not harmed.
* No license has been chosen yet. Choose one before publishing.
