# Security

## What is protected, from whom

| Adversary | Outcome |
|---|---|
| Reads the message bus (operator, anyone with read access) | ciphertext of uniform size; keys never touch the bus |
| Publishes to the bus (forge, inject) | every forged message is rejected; none can move a live session (tested with 200 to 600 forged packets, half with fresh keys in the header) |
| Replays captured messages or handshakes | rejected: message keys are single use, hellos carry a fresh nonce and a time window |
| Delays, reorders, drops messages | expired commands are dropped and reported; reordering is handled; drops leave gaps, not corruption |
| Steals an agent's or the hub's session state later | cannot read earlier messages (per-message forward secrecy); locked out again after one more exchange (post-compromise security) |
| Records traffic now and has a quantum computer later | the handshake mixes in ML-KEM-768, so recorded sessions stay safe unless both X25519 and ML-KEM fall |
| Steals only a signing key | can forge a signed ack but cannot derive the session key without the static DH key |
| Takes over the chat account | can issue commands the hub relays; cannot disarm an alarm protected by a one-time code, cannot unlock a locked agent |
| Reads the logs | no message text, no token; errors are stripped of URLs that contain the token |

## What is not protected

* **The chat leg.** Bot chats are not end-to-end encrypted and the hub sees plain text. Whoever controls the hub while it
  runs can read and relay. Treat the hub's host as sensitive.
* **A compromised agent machine** can do what the agent can.
* **Availability.** Anyone who can publish to the bus can fill it with junk; the receiver drops it cheaply, but it costs
  bus quota. Use IAM so that only the right service accounts can publish.
* **Traffic analysis.** Sizes are padded to 1 KiB buckets and heartbeats are regular, but the timing of real use is visible.
* **Approval of individual tool calls** by one-time code is a library (`internal/approval`) used for the alarm; it is not
  yet connected to an assistant's per-tool hook.

## Rules the code follows

1. Nothing is trusted because of an attribute, a sender field or a session id. Only a message that authenticates under the
   session counts.
2. Decrypt commits state only after authentication succeeds.
3. Everything an attacker can make the receiver store or compute is bounded (skipped keys, seen ids, pending handshakes,
   hello nonces, message sizes, spool length).
4. There is no fallback to a weaker mode. An unknown version is rejected; the agent never switches to another channel on
   its own.
5. Time is used only to expire commands and to window handshakes; replay protection uses counters and nonces.
6. Private keys and secrets live in files that must be mode 0600; the programs refuse to read them otherwise. Secrets are
   never accepted as command-line flags.
7. Text is typed into tmux literally after `--`, limited to 8 KiB, with control characters refused; keys come from an
   allow-list.
8. Logs record event names, ids and sizes, never content.

## Tests that correspond to attacks

| Attack | Tests |
|---|---|
| Forged packets, fresh-key headers, state corruption | `ratchet.TestForgeriesNeverBreakTheSession`, `TestLowOrderKeyInHeaderIsRejected`, `FuzzDecrypt`, `e2e.TestForgedFloodNeverBreaksTheSession`, `e2e.TestEmulatorReplayAndInjectionAreHarmless` |
| Any altered byte, wrong direction or session | `ratchet.TestEveryByteIsAuthenticated`, `TestWrongAssociatedDataIsRejected`, `wire.TestMessageCannotBeMovedToAnotherDirectionOrSession`, `wire.TestDirectionIsEnforced` |
| Replay of messages | `ratchet.TestReplayIsRejected`, `e2e.TestReplayOfEveryCapturedMessageIsHarmless`, `e2e.TestReplayOfAgentMessagesToTheHub` |
| Replay or forgery of handshakes | `handshake.TestEveryFieldIsSigned`, `TestReplayedHelloIsRejected`, `TestGuardOnlyRecordsAuthenticatedHellos`, `TestAckMustAnswerOurHello`, `TestAckFromAnImpostorIsRejected`, `TestKindConfusionAndReflection`, `TestStaleAndFutureHellosAreRejected`, `e2e.TestReplayedRealHelloCannotResetTheSession`, `e2e.TestForgedHellosAreRefusedAndTheSessionSurvives` |
| Stolen signing key, degenerate keys, the KEM | `handshake.TestStolenSigningKeyAloneDoesNotGiveTheSession`, `TestDegenerateEphemeralKeyIsRejected`, `TestKEMSecretContributesToTheKey` |
| Resource exhaustion | `ratchet.TestSkipLimit`, `TestStoredSkippedKeysAreBounded`, `handshake.TestGuardIsBoundedAndForgets`, `spool.TestSizeAndCountLimits` |
| Forward secrecy and healing | `ratchet.TestForwardSecrecy`, `TestPostCompromiseSecurity`, `TestRestoringAnOldStateBreaksTheSession` |
| Stale and reordered commands | `e2e.TestDelayedCommandsExpireInsteadOfRunning`, `TestReorderedCommandsAreAllExecutedOnce`, `TestLostMessagesDoNotStopLaterOnes` |
| Unauthorised chat sender | `e2e.TestOnlyTheAuthorisedUserIsObeyed` |
| One agent acting on another's messages | `e2e.TestAgentsCannotReadOrActForEachOther`, `TestAWrongSessionCommandIsRefused` |
| Lockdown and local unlock | `e2e.TestLockdownIsEasyToStartAndNeedsALocalUnlock`, `TestLockSurvivesAnAgentRestart`, `TestHubLockdownStopsRelayingEvenToAnUnlockedAgent` |
| Silencing the alarm | `e2e.TestDisarmingCanRequireAOneTimeCode`, `TestAReplayedDisarmCommandDoesNotSilenceAReArmedAlarm`, `TestADelayedDisarmCommandExpires`, `TestALockedMachineKeepsItsAlarmArmed`, `approval.TestACodeWorksOnlyOnce`, `TestLockoutAfterRepeatedFailures` |
| Lost replies and alerts | `e2e.TestSpoolSurvivesAHubOutageAndIsDeliveredLater`, `TestAnAlertSurvivesAHubOutage`, `TestAnAlertSurvivesAChatOutage`, `e2e.TestEmulatorReplyWrittenDuringAHubOutageIsDelivered` |
| Input into tmux | `tmuxctl.TestSendTextIsLiteralAndSeparatedByDoubleDash`, `TestBadTextIsRefusedBeforeTmuxRuns`, `TestKeyAllowList` |
| Secrets in logs and errors | `telegram.TestTheTokenNeverAppearsInErrors`, `smoke.TestRealBinariesEndToEnd` (checks the real binaries' logs), `config.TestTheTokenIsNeverAFlag`, `identity.TestLoadRefusesWorldReadableFile` |

## Reporting a problem

Please report security problems privately to the maintainer before publishing them.
