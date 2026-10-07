# Security

## What is protected, from whom

| Adversary | Outcome |
|---|---|
| Reads the message bus (operator, anyone with read access) | ciphertext of uniform size; keys never touch the bus |
| Publishes to the bus (forge, inject) | every forged message is rejected; none can move a live session (tested with 200 to 600 forged packets, half with fresh keys in the header) |
| Replays captured messages or handshakes | rejected: message keys are single use, hellos carry a fresh nonce and a time window |
| Delays, reorders, drops messages | expired commands are dropped and reported; the hub numbers commands and the agent runs them in that order, waits a short window for a missing one, then reports it lost; a command that arrives after its turn is refused; drops leave gaps, not corruption |
| Steals an agent's or the hub's session state later | cannot read earlier messages (per-message forward secrecy); locked out again after one more exchange (post-compromise security) |
| Records traffic now and has a quantum computer later | the handshake mixes in ML-KEM-768, so recorded sessions stay safe unless both X25519 and ML-KEM fall |
| Steals only a signing key | can forge a signed ack but cannot derive the session key without the static DH key |
| Takes over the chat account | can issue commands the hub relays; cannot disarm an alarm protected by a one-time code, cannot unlock a locked agent |
| Reads the logs | no message text, no token; errors are stripped of URLs that contain the token |

## What is not protected

* **The chat leg.** Bot chats are not end-to-end encrypted and the hub sees plain text. Whoever controls the hub while it
  runs can read and relay. Treat the hub's host as sensitive.
* **A compromised agent machine** can do what the agent can.
* **Another process of the same user on the agent machine.** It can read the agent's identity, change the lock file, type
  into the tmux session itself and read the spool. Files are mode 0600 and the state directory 0700, which keeps other
  users out, not the same user. Run the agent under an account that runs nothing you do not trust.
* **Availability.** Anyone who can publish to the bus can fill it with junk. The receiver bounds what it holds (32
  messages and 16 MiB in flight) and spends a key agreement on at most 20 new peer keys in a burst, then 5 a second, so
  junk cannot burn its CPU; but it costs bus quota, and while the budget is empty a real new-key message can be refused.
  Use IAM so that only the right service accounts can publish.
* **Traffic analysis.** Sizes are padded to 1 KiB buckets and heartbeats are regular, but the timing of real use is visible.
* **Approval of individual tool calls** by one-time code is a library (`internal/approval`) used for the alarm; it is not
  yet connected to an assistant's per-tool hook.

## Rules the code follows

1. Nothing is trusted because of an attribute, a sender field or a session id. Only a message that authenticates under the
   session counts.
2. Decrypt commits state only after authentication succeeds.
3. Everything an attacker can make the receiver store or compute is bounded (skipped keys, seen ids, pending handshakes,
   hello nonces per sender, message sizes, spool length and file size, queued commands, new-key agreements per second).
   Cheap checks come first: a hello's signature is checked before it counts against anything.
4. There is no fallback to a weaker mode. An unknown version is rejected; the agent never switches to another channel on
   its own.
5. Time is used only to expire commands and to window handshakes; replay protection uses counters and nonces.
6. Private keys and secrets live in files that must be mode 0600; the programs refuse to read them otherwise
   (`PB_ALLOW_GROUP_READ=true` also accepts 0640, for a Kubernetes secret volume with `fsGroup`). Secrets are never
   accepted as command-line flags. The agent's state directory is 0700.
9. Locks fail closed. A lock file that exists, or whose state cannot be checked, means locked; a lockdown that cannot be
   written to disk still holds in memory and the hub is told so. The alarm's state file is written before the alarm
   changes state, and disarming removes it first, so a failure leaves the alarm armed.
10. A command's lifetime starts when the user typed it (the chat's message date), not when the hub got round to it. A
    backlog from a chat outage is not acted on, except `/lockdown`.
11. A used one-time code stays used across a restart of the agent, and so does a lockout.
12. With several agents the hub never guesses where a command goes. Until the user chooses an agent (or names one with
    `@name`, or answers one of its messages) nothing is sent, and a restart of the hub forgets the choice. A tap on an agent
    button counts only from the configured chat and user.
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
| Resource exhaustion | `ratchet.TestSkipLimit`, `TestStoredSkippedKeysAreBounded`, `TestAFloodOfFreshKeysIsCutOffBeforeItCostsAnything`, `TestARealKeyStepWaitsForTheBudgetAndThenWorks`, `handshake.TestGuardIsBoundedAndForgets`, `TestOneSenderCannotFillTheGuardForTheOthers`, `TestVerifyHelloRecordsNothing`, `spool.TestSizeAndCountLimits`, `TestOversizedAndCorruptFilesAreSetAside`, `agentsvc.TestTheQueueCannotBeMadeToGrowWithoutLimit`, `hubsvc.TestAStrangersMessagesAreCountedButDoNotFloodTheLog` |
| Forward secrecy and healing | `ratchet.TestForwardSecrecy`, `TestPostCompromiseSecurity`, `TestRestoringAnOldStateBreaksTheSession` |
| Stale, reordered, lost and late commands | `e2e.TestDelayedCommandsExpireInsteadOfRunning`, `TestCommandsRunInTheOrderTheyWereTyped`, `TestKeysAndTextShareOneOrder`, `TestALostCommandIsReportedAndTheNextOnesStillRun`, `TestACommandThatArrivesAfterItsTurnIsNotRun`, `TestOldChatMessagesAreNotActedOn`, `TestLockdownIsObeyedWhateverItsAge`, `TestAnOldUnlockIsIgnored`, `TestEmulatorRapidCommandsRunInOrder`, `agentsvc.TestCommandsRunInTheHubsOrderWhateverTheArrivalOrder`, `TestAGapHoldsLaterCommandsForTheWindowAndThenIsReported`, `TestACommandWithoutANumberIsRefused`, `TestExpiryIsCheckedWhenACommandIsAboutToRunNotWhenItArrived` |
| Unauthorised chat sender | `e2e.TestOnlyTheAuthorisedUserIsObeyed` |
| One agent acting on another's messages | `e2e.TestAgentsCannotReadOrActForEachOther`, `TestAWrongSessionCommandIsRefused` |
| Lockdown and local unlock | `e2e.TestLockdownIsEasyToStartAndNeedsALocalUnlock`, `TestLockSurvivesAnAgentRestart`, `TestHubLockdownStopsRelayingEvenToAnUnlockedAgent`, `TestALockThatCannotBeWrittenStillHoldsAndIsReported`, `TestALockWhoseFileCannotBeCheckedFailsClosed`, `TestAHubLockdownIsRepeatedToAnAgentThatReconnects`, `agentsvc.TestLockIsCheckedWhenACommandIsAboutToRunNotWhenItArrived` |
| Session setup races | `e2e.TestSeveralHellosInFlightStillGiveOneWorkingSession`, `TestReadyIsRepeatedWhenTheFirstOneIsLost` |
| Silencing the alarm | `e2e.TestDisarmingCanRequireAOneTimeCode`, `TestAReplayedDisarmCommandDoesNotSilenceAReArmedAlarm`, `TestADelayedDisarmCommandExpires`, `TestALockedMachineKeepsItsAlarmArmed`, `approval.TestACodeWorksOnlyOnce`, `TestLockoutAfterRepeatedFailures`, `TestAUsedCodeStaysUsedAfterARestart`, `TestALockoutSurvivesARestart`, `TestACodeWhoseUseCannotBeRecordedIsRefused`, `alarm.TestArmingTakesTheBaselineOnlyWhenItWasOff`, `TestArmFailsCleanlyWhenTheStateFileCannotBeWritten`, `TestDisarmFailsSafeWhenTheStateFileCannotBeRemoved` |
| Lost replies and alerts | `e2e.TestSpoolSurvivesAHubOutageAndIsDeliveredLater`, `TestAnAlertSurvivesAHubOutage`, `TestAnAlertSurvivesAChatOutage`, `e2e.TestEmulatorReplyWrittenDuringAHubOutageIsDelivered` |
| Input into tmux | `tmuxctl.TestSendTextIsLiteralAndSeparatedByDoubleDash`, `TestBadTextIsRefusedBeforeTmuxRuns`, `TestKeyAllowList`, and against the real tmux `TestRealTmuxTypesTheTextExactly` (a trailing `;` or `\;`, option-like text, shell syntax, unicode), `TestRealTmuxDoesNotInterleaveConcurrentSenders` |
| Secrets in logs and errors | `telegram.TestTheTokenNeverAppearsInErrors`, `TestAStalledConnectionDoesNotHangTheHub`, `smoke.TestRealBinariesEndToEnd` (checks the real binaries' logs), `config.TestTheTokenIsNeverAFlag`, `TestBotAPIAddressMustNotSendTheTokenInTheClear`, `TestGroupReadableSecretsNeedTheExplicitSetting`, `identity.TestLoadRefusesWorldReadableFile`, `TestGroupReadIsAllowedOnlyWhenAskedFor` |

## Reporting a problem

Please report security problems privately to the maintainer before publishing them.
