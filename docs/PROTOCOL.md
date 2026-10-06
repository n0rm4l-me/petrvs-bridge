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
  bytes), info = `claudebridge session v1`). The three Diffie-Hellman results bind both ephemeral and both static keys; the
  ML-KEM secret protects against a later quantum computer; the transcript in the salt binds everything that was signed.
* Session id = the first 8 bytes of SHA-256(`claudebridge session id v1` || transcript).

The responder's ephemeral key is also its first ratchet key. The hub cannot send until it has received the agent's first
message (`ready`), as in Signal's initial message flow.

A new handshake replaces a session only after the new session has carried a valid message. Until then the hub tells users
"reconnecting" and sends nothing, instead of sending into a session that is being replaced.

## Ratchet

* DH ratchet: X25519. Root step: HKDF-SHA256(salt = root key, ikm = DH, info = `claudebridge ratchet root v1`) giving a
  new root key and a chain key.
* Chain step: HMAC-SHA256 with the chain key over `0x02` for the next chain key and `0x01` for the message key.
* Message key -> HKDF (`claudebridge message v1`) -> AES-256-GCM key (32 bytes) and nonce (12 bytes). A key is used once.
* Header (plaintext but authenticated): ratchet public key (32), previous chain length (4), message number (4).
* Associated data = header || caller data (`claudebridge msg v1` || sender || recipient || session id), so a message cannot
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
{ "id": "...", "type": "text", "body": "...", "ts": 1800000000, "exp": 1800000600, "meta": {} }
```

Parsed strictly (unknown fields rejected). Allowed types depend on the direction, enforced on receipt:

| Direction | Types |
|---|---|
| hub to agent | `text`, `key`, `lockdown`, `alarm`, `ping`, `hback`, `rcpt` |
| agent to hub | `ready`, `reply`, `notice`, `alert`, `hb`, `dropped`, `pong` |

* `exp`: a command that reaches the agent after its expiry is dropped and reported. The hub sets 10 minutes for text, 1
  minute for keys and alarm commands, counted from the time the user typed the message (the chat's message date), and
  the agent checks it again at the moment it is about to run the command.
* `meta.n`: on `text` and `key`, the number of the command in this session, from 1. The agent runs commands in that
  order, one at a time. A missing number is waited for `CB_REORDER_WINDOW` (2 s by default) and then reported as lost; a
  command whose number has already been passed is refused and reported; a command without a number is refused.
* `id`: remembered per session (1024 entries) to drop duplicates.
* Replies, notices and alerts are queued in a spool on the agent, sent with a stable id, and the file is removed only
  after the hub answers with `rcpt` carrying that id. The hub sends the receipt only after the chat accepted the message,
  and repeats the receipt (without repeating the message) for a resend it has already shown.

## Liveness

The agent sends `ready` as the first message of a session and repeats it every handshake interval until the hub has
answered anything (the hub answers `ready` with `hback`). The hub starts using a new session when it first hears from
it, and keeps up to four offered sessions per agent until one of them is proved. The agent sends `hb` every heartbeat
interval; the hub answers `hback`. An agent that hears nothing from the hub for the
silence limit starts a new handshake (this is how a hub restart is healed). The hub calls an agent offline after a period
without any authenticated message and says so once.

## Lockdown

`lockdown` makes the agent refuse `text` and `key` and refuse to disarm its alarm; it creates a lock file that only a local
`agent unlock` removes, and answers with a `notice` saying whether the lock reached the disk (if not, it holds until the
agent restarts). The hub stops relaying at the same time; `/unlock` on the hub does not unlock agents. With
`CB_LOCK_FILE` the hub's own lockdown survives a restart, and a locked-down hub repeats `lockdown` to every agent whose
new session it accepts.
