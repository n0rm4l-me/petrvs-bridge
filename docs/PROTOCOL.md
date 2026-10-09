# Protocol

Everything between hub and agent travels as Pub/Sub messages. Attributes are plaintext routing hints and are **never**
trusted; a message is acted on only if it authenticates.

## Identities

Each party has a long-term X25519 key (used in the handshake) and an Ed25519 key (signs handshake messages). They never
encrypt traffic. The other side pins the public halves (`NAME.pub.json`) when it is enrolled; nobody else can connect.
The fingerprint is SHA-256 over a domain string, the name and both public keys, shown as 10 groups of 4 hex characters.

## Handshake (agent initiates)

```
agent -> hub   hello  { v, kind, from, to, eph(32), kem(1184), nonce(16), ts, sig }
hub   -> agent ack    { v, kind, from, to, eph(32), kem(1088), nonce(16), ts, ref = SHA-256(hello signed bytes), sig }
```

* `eph` is a fresh X25519 public key, `kem` in the hello is an ML-KEM-768 encapsulation key and in the ack the ciphertext
  for it. The private halves exist only in memory.
* `sig` is an Ed25519 signature over every field with a domain prefix and length-prefixed fields.
* A receiver checks the shape, that `to` is itself and `from` is the pinned peer, the signature, and that `ts` is within
  two minutes of its clock. The hub also refuses a hello whose nonce it has seen (a bounded memory that only
  authenticated hellos can fill) and limits how often one agent may start a handshake.
* Session key = HKDF-SHA256(ikm = DH(eI,eR) || DH(eI,sR) || DH(sI,eR) || mlkem_secret, salt = SHA-256(hello || ack signed
  bytes), info = `petrvs-bridge session v1`). The three Diffie-Hellman results bind both ephemeral and both static keys; the
  ML-KEM secret protects against a later quantum computer; the transcript in the salt binds everything that was signed.
* Session id = the first 8 bytes of SHA-256(`petrvs-bridge session id v1` || transcript).

The responder's ephemeral key is also its first ratchet key. The hub cannot send until it has received the agent's first
message (`ready`), as in Signal's initial message flow.

A new handshake replaces a session only after the new session has carried a valid message. Until then the hub tells users
"reconnecting" and sends nothing, instead of sending into a session that is being replaced.

## Ratchet

* DH ratchet: X25519. Root step: HKDF-SHA256(salt = root key, ikm = DH, info = `petrvs-bridge ratchet root v1`) giving a
  new root key and a chain key.
* Chain step: HMAC-SHA256 with the chain key over `0x02` for the next chain key and `0x01` for the message key.
* Message key -> HKDF (`petrvs-bridge message v1`) -> AES-256-GCM key (32 bytes) and nonce (12 bytes). A key is used once.
* Header (plaintext but authenticated): ratchet public key (32), previous chain length (4), message number (4).
* Associated data = header || caller data (`petrvs-bridge msg v1` || sender || recipient || session id), so a message cannot
  be moved to the other direction, another session or another party.
* Padding: ISO/IEC 7816-4 to a multiple of 1024 bytes; at most 1 MiB of plaintext.
* Out-of-order delivery: skipped message keys are stored, at most 50 per gap and 200 in total.
* **Decrypt is atomic.** It works on a copy of the state and commits only if the authentication tag verifies. A forged
  packet, however shaped, changes nothing.

## Framing

Pub/Sub data = `version(1) | session id(8) | ratchet message`. Attributes: `v`, `kind` (`hello`, `ack`, `msg`), `agent`.
Handshake messages are JSON (strictly parsed, at most 8 KiB).

## Payload (inside the ratchet message)

```json
{ "id": "...", "type": "text", "body": "...", "ts": 1800000000, "exp": 1800000600, "meta": {}, "buttons": [] }
```

Parsed strictly (unknown fields rejected). Allowed types depend on the direction, enforced on receipt:

| Direction | Types |
|---|---|
| hub to agent | `text`, `key`, `file`, `start`, `screen`, `part`, `lockdown`, `alarm`, `ping`, `hback`, `rcpt` |
| agent to hub | `ready`, `reply`, `notice`, `alert`, `part`, `file`, `hb`, `dropped`, `pong`, `done` |

* `exp`: a command that reaches the agent after its expiry is dropped and reported. The hub sets 10 minutes for text, 1
  minute for keys and alarm commands, counted from the time the user typed the message (the chat's message date), and
  the agent checks it again at the moment it is about to run the command.
* `meta.n`: on the commands `text`, `key`, `file` and `start`, the number of the command in this session, from 1. The agent runs commands in that
  order, one at a time. A missing number is waited for `PB_REORDER_WINDOW` (2 s by default) and then reported as lost; a
  command whose number has already been passed is refused and reported; a command without a number is refused. The hub
  gives the number back when it cannot seal the command, and publishes a command up to three times, 300 ms apart, with
  the same bytes (the agent drops a copy it has already seen); it does not give the number back after a publish.
* `done`: sent by the agent after it has carried out a command (typed a `text`, pressed a `key`, saved a `file`, done a
  `start`), and for each `part` of a file it has stored; `body` is the `id` of that command or part. It is not
  spooled (if it is lost only a chat mark is lost). The hub uses it to replace the "read" mark on the user's chat message
  with the "delivered" mark, and only when it comes from the agent that was sent the command.
* `buttons`: on a `notice` or `reply` from the agent, up to 8 of `{l: label (at most 40 bytes), k: key (at most 16),
  p: prompt hash (at most 16)}`. The hub shows them under the message. A press is only a request: the hub sends the key
  as a `key` command (callback data `k:<agent>:<key>:<prompt>`, or `s:<agent>` for the key `screen`, which asks for a
  snapshot of the screen), after checking the sender, the agent, and that the key is one of Enter, Escape, Up, Down,
  Tab, 1 to 9. `BTab` is never sent from a button.
* `meta.prompt` on a `key`: the hash of the prompt the key answers: 16 hex characters, HMAC-SHA256 keyed with the install's
  `prompt-key` (in the agent's state directory), over the dialog as drawn before anything is hidden or cut. Lines in the
  command's frame (starting with the box bar) are taken verbatim, blank lines count, and only the choices (cursor marker and
  indentation) and the tip line are normalised, so the hash does not depend on which choice is highlighted. The choices are
  the last numbered run of at least two entries in the dialog. The agent reads the screen of the pane and presses the key
  only if a prompt with that hash is shown and the key is Escape or a digit within its choices; otherwise it reports
  `dropped`. A key without a hash carrying `meta.echo` "screen" (the navigation buttons) is refused for Enter and digits
  while a prompt is on the screen. A prompt whose text had to be shortened is sent with an Esc button only.
* The hub sends a key or screen request from a button only for a message it sent with buttons less than 15 minutes ago;
  otherwise it answers "Those buttons have expired; use /screen." The record is in memory, so after a hub restart old
  buttons are refused. A tap that could not be published is answered "Not sent"; the buttons stay and the message can be
  tapped again.
* `meta.echo` on a `key`: `mode` asks the agent to report the permission mode afterwards (used for `BTab`); `screen` asks
  for a snapshot of the screen afterwards. On a `text` it is `screen` too (`/resume`).
* `screen`: the hub asks for a snapshot of the screen. The agent answers with a `notice` (the prompt with its buttons, or
  the bottom 25 lines with navigation buttons), at most one a second, and not at all when locked or expired.
* `start`: a command. The agent starts Claude Code in its tmux session if it is not running there (creates the session, or
  types `cd DIR && exec COMMAND` into the shell), using only its own settings; or says that it is already running.
* `dropped` may carry `meta.n`, the number of a command that never arrived. The hub leaves such a report out when it has
  already told the user that it could not publish that command. When a session is replaced, each command waiting in the
  agent's queue is reported with a `dropped` (at most 256).
* `id`: remembered per session (1024 entries) to drop duplicates.
* Replies, notices and alerts are queued in a spool on the agent, sent with a stable id, and the file is removed only
  after the hub answers with `rcpt` carrying that id. The hub sends the receipt only after the chat accepted the message,
  or refused it for good (HTTP 400: logged by id and size, then dropped; any other failure leaves the message queued),
  and repeats the receipt (without repeating the message) for a resend it has already shown. A resend that arrives while
  the first copy is still being written to the chat is dropped without a receipt.

### Files

A file travels as `part` payloads followed by one `file` payload. A `part` carries at most 128 KiB in `body` (base64)
with `meta.t` (the transfer id, 1 to 32 characters) and `meta.i` (the index from 0). The `file` payload ends the transfer:
`body` is the caption (at most 1000 bytes); `meta` has `t`, `parts`, `name`, `size` (bytes), `sha` (SHA-256 of the whole,
hex) and `kind` (`photo` or `document`). Nothing is trusted before the whole has been put together and its size and hash
match; the receiver holds at most 16 unfinished transfers, twice the size limit in total and forgets an idle one after ten
minutes. The ratchet skips at most 50 message keys when the bus reorders and gives a receiver a budget of new peer keys
(`ratchet.DHStepBurst` = 20, refilled at `DHStepPerSecond` = 5), so both directions pace their parts: a sender keeps at most
16 unconfirmed parts and starts a new batch at most once per 250 ms (`MinTurn`), when at least half of the window is free
or everything that remains fits. All messages of a batch are sealed before the first is published, so the ratchet sends a
whole batch under one key: answers that arrive while a batch is going out would otherwise change the key for almost every
part, and each new key costs the receiver one step of its budget. The limits themselves are not changed. A message refused
for the budget is acknowledged and lost; both ends log that at Warn, at most once per 10 s with a count.

*Machine to chat.* `agent send-file` writes the parts and the closing item to the spool. The agent sends them like replies,
at most 16 unconfirmed at a time, and sends the closing item only after the parts of its own transfer were confirmed;
while the machine is locked they stay. The hub receipts a part when it is stored (or unusable). It does not receipt the closing item while parts
are missing; after 3 tries or 2 minutes it reports the loss in the chat and receipts. It receipts after Telegram has
taken the file, or after a permanent refusal (HTTP 400, 413). A transient failure withholds the receipt, and the agent
resends; after 5 failures or 5 minutes the hub gives up and says so. Uploads go one at a time. Every give-up (damaged,
too large, refused by Telegram, parts missing, cannot upload) is said to the chat first and receipted only after the chat
heard it (or refused it with a 400); if the chat is down the agent sends the item again and the next try repeats the
verdict. An upload that could not be tried to the end stays in the hub's memory and is tried again on the next resend.
The hub takes up to 16 transfers from an agent at once.

*Chat to machine.* The hub fetches the file from Telegram and sends `part` payloads (not commands, not numbered) with a
window of 16 parts that the agent has not yet answered, in batches as described above. The agent answers each stored part with a `done` whose `body` is the
part's payload id; the hub gives up after 10 s without an answer. The hub then sends `file` as a numbered command, so it is
ordered with the user's text and keys. The agent waits up to 10 s for missing parts, checks size and SHA-256, saves the file
in its inbox and types a line about it into the session. At most 2 transfers run at a time.

*Order of commands.* Commands get their number when the hub sends them, which for a file is when the transfer ends. So that
what the user typed after a photo is not typed before it, the hub keeps a line per agent: the place of a file is taken when
its message is read, and every text, key or `/continue` for that agent that arrives after it is held in order (at most 32
per agent, the file's place included) and sent on when the closing `file` command is sent or the transfer fails. With
several files on the way the line also keeps their order. `/lockdown` discards what is held; screen requests and alarm
commands are not held. A held command keeps its time to live; one that is too old when its turn comes is reported as such.

## Liveness

The agent sends `ready` as the first message of a session and repeats it every handshake interval until the hub has
answered anything (the hub answers `ready` with `hback`). The hub starts using a new session when it first hears from
it, and keeps up to four offered sessions per agent until one of them is proved. The agent sends `hb` every heartbeat
interval; the hub answers `hback`. An agent that hears nothing from the hub for the
silence limit starts a new handshake (this is how a hub restart is healed). A subscription call that fails (a token that could not be refreshed because the laptop was asleep or off the network, a stream that was reset) is not the end of the agent: it logs a warning, waits (1 s, doubling to a minute, starting over after a stretch that went well) and reads again. The hub calls an agent offline after a period
without any authenticated message and says so once (again later if the chat was down at that moment). The next
authenticated message of any type from an offline agent is answered with "is back" ("connected" for `ready`).

The heartbeat's `meta` carries facts for the status card (each at most 100 bytes, one line, at most 16 entries): `locked`,
`alarm` (`on` or `off`), `host`, `os`, `bat` ("83% charging"), `up` ("3d4h"), `disk` ("212G free of 460G"), `tmux` (`ok` or
`missing`), `cc` (the program in the foreground of the active pane), `mode` (the permission mode read from the screen) and `q`
(the length of the spool). The hub cleans and bounds them before showing them. The agent reads tmux at most every 30 s
for them, and again at once when the pane changes between Claude Code, a shell and nothing.

## Lockdown

`lockdown` makes the agent refuse `text` and `key` and refuse to disarm its alarm; it creates a lock file that only a local
`agent unlock` removes, and answers with a `notice` saying whether the lock reached the disk (if not, it holds until the
agent restarts). The hub stops relaying at the same time; `/unlock` on the hub does not unlock agents. With
`PB_LOCK_FILE` the hub's own lockdown survives a restart, and a locked-down hub repeats `lockdown` to every agent whose
new session it accepts, and to an agent whose heartbeat says `locked=false` while the hub is locked, at most every 10 s
per agent. The `/lockdown` reply counts only the agents the publish reached and names the others.
