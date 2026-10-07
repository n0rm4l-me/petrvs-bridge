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

Status: working and tested (unit tests, fuzzing, end-to-end tests on the Pub/Sub emulator, and a smoke test with the real
binaries, a real tmux and a fake chat server). It is young, the protocol is custom, and it has **not had an independent
security audit**. Read the limitations before trusting it with anything that matters.

## Quick start on one machine (Pub/Sub emulator)

You need Go 1.26.6 or newer (the `go` line in `go.mod` makes Go fetch a fixed toolchain by itself), tmux, and for the emulator `gcloud` with `gcloud components install pubsub-emulator` (and Java).

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
| answer (swipe-reply) to a message of an agent | your text, or a key command, goes to the agent that wrote that message, whatever is active; the hub notes the destination when it differs from the active agent |
| `/agents` or `/status` | agents, online or offline, last heard, facts from their heartbeat |
| `/agent NAME` | choose the active agent. `/agent` alone, `/agents` and `/status` show buttons, one per agent, the active one marked with ●; a tap chooses |
| `/enter` `/esc` `/up` `/down` `/tab`, `/key N` | press a key (`N` is 1 to 9) |
| `/alarm on` | arm the alarm (see below) |
| `/alarm off [code]` | disarm; the code is needed if the agent has an alarm secret |
| `/alarm` or `/alarm status` | state and which sensors are working |
| `/lockdown` | the hub stops relaying and tells agents to refuse text and keys until unlocked **on their own machine**. Always obeyed, whatever the message's age |
| `/unlock` | the hub relays again (agents stay locked) |
| `/help` or `/start` | the list of commands |

Only the chat id and user id you configure are obeyed; everyone else is ignored and counted.

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
  machine that is not the chosen one is answered where it was asked. The hub remembers the last 512 messages for this.
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
list every problem at once and exit.

### Hub (`bin/hub`)

| Variable | Flag | Default | Meaning |
|---|---|---|---|
| `PB_IDENTITY` | `-identity` | required | Path of the hub's private identity file (mode 0600) |
| `PB_AGENTS_DIR` | `-agents-dir` | required | Directory with one `NAME.pub.json` per agent that may connect. Nobody else can |
| `TELEGRAM_BOT_TOKEN` | none | required (or the next) | Bot token |
| `PB_TELEGRAM_TOKEN_FILE` | none | | File holding the bot token (mode 0600). Used if the variable above is unset |
| `PB_CHAT_ID` | `-chat-id` | required | The only chat that is obeyed |
| `PB_USER_ID` | `-user-id` | the chat id | The only user that is obeyed (in a private chat it equals the chat id) |
| `PB_PROJECT` | `-project` | required unless the emulator is used | Pub/Sub project id (`GOOGLE_CLOUD_PROJECT` is also read) |
| `PB_PREFIX` | `-prefix` | `pb` | Prefix of topic and subscription names |
| `PB_CREATE_RESOURCES` | `-create-resources` | `false` | Create topics and subscriptions. For development and the emulator only; create them with infrastructure code in production |
| `PB_TELEGRAM_API_BASE` | `-telegram-api` | `https://api.telegram.org` | Bot API address (a local Bot API server, or a test double) |
| `PB_OFFLINE_AFTER` | `-offline-after` | `3m` | Silence after which an agent is called offline |
| `PB_TEXT_TTL` | `-text-ttl` | `10m` | How long a text command stays valid |
| `PB_KEY_TTL` | `-key-ttl` | `1m` | How long a key press or an alarm command stays valid |
| `PB_POLL_TIMEOUT` | `-poll-timeout` | `25s` | Telegram long-poll timeout |
| `PB_MAX_REPLY_BYTES` | `-max-reply-bytes` | `20000` | Longest reply shown in the chat (longer ones are cut, with a note) |
| `PB_DEFAULT_AGENT` | `-default-agent` | unset | Agent that gets plain text until you choose one. Unset with several agents means the hub asks first; with one agent that one is used |
| `PB_DELIVERED_REACTION` | `-delivered-reaction` | `👍` | Emoji that replaces the first one when the agent has typed the text (or pressed the key) in its session; `off` leaves the first one. Same rules as above |
| `PB_RECEIVED_REACTION` | `-received-reaction` | `👀` | Emoji the hub puts on each of your messages once it has read it; `off` turns it off. Telegram accepts only a fixed set of reaction emoji (see `ReactionTypeEmoji` in the Bot API documentation, for example 👀 👍 🔥 🎉 🫡 🤝 ⚡); if it refuses yours the hub says so once in its log and stops marking |
| `PB_LOCK_FILE` | `-lock-file` | unset | File that keeps the hub's lockdown across restarts (in Kubernetes: a path on an `emptyDir`) |
| `PB_ALLOW_GROUP_READ` | `-allow-group-read` | `false` | Also accept private files that are group-readable (0640), for a Kubernetes secret volume with `fsGroup`. Never world-readable |
| `PB_LOG_LEVEL` | `-log-level` | `info` | `debug`, `info`, `warn`, `error`. Logs never contain message text or tokens |
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
| `PB_ALLOW_GROUP_READ` | `-allow-group-read` | `false` | As for the hub |
| `GOOGLE_APPLICATION_CREDENTIALS`, `PUBSUB_EMULATOR_HOST` | | | As for the hub |

Other agent commands: `agent send [-type reply|notice|alert] [TEXT | -]` queues a message for the hub (reads standard
input for `-`); it only needs `PB_STATE_DIR`. `agent unlock` lifts a lockdown, and is the only way to do it.

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
an answer is not lost if the network or the hub is down when it is written.

## Development

```sh
make test              # unit tests and the in-memory end-to-end tests, with the race detector
make test-emulator     # everything, including the Pub/Sub emulator tests and the real-binary smoke test
make fuzz              # 30 seconds of fuzzing of the ratchet's decrypt
make cover
make vuln            # govulncheck: known vulnerabilities in the code paths this program uses
```

`scripts/with-emulator.sh CMD` runs a command with an emulator available (it starts one with gcloud if
`PUBSUB_EMULATOR_HOST` is unset). Tests that need the emulator are skipped without it.

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
* `examples/` and `deploy/` are provided as starting points; the Kubernetes and systemd files have not been run.
* A flood of forged packets with new keys is cut off cheaply, but while the receiver's budget of new-key agreements
  (20 burst, 5 a second) is empty a real message that needs a new key is refused until it refills. The session is not harmed.
* No license has been chosen yet. Choose one before publishing.
