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
| Writes to the app in Slack from another account, channel or workspace | counted and ignored: the Slack client passes on only the one user, in the direct message with the app, in the workspace of the bot; the hub then checks the same again |
| Puts a mention or a link in text that the agent shows (`<!channel>`, `<@U123>`, `<https://evil|x>`) | cannot ping anyone or make a link: every `&`, `<` and `>` is escaped, and a link is made only from a Markdown link to http, https or mailto that passed a check; its label is plain text and the address follows it as written (`label (https://...)`), so a label cannot name another place than the link goes to; text that reads backwards (direction overrides) is cleaned |

## What is not protected

* **The chat leg.** Bot chats are not end-to-end encrypted and the hub sees plain text. Whoever controls the hub while it
  runs can read and relay. Treat the hub's host as sensitive. This includes what the `Notification` example hook puts in the chat: the
  permission prompt as drawn on the screen, commands and questions in full. Values after words like token or password are
  hidden before it is sent, but that is best effort (a notice says "(some values are hidden)" when it did so). A dialog
  over 1800 characters loses its middle, is marked "[N characters left out]" and gets only an Esc button and the advice to
  read it on the laptop or with `/screen`: what cannot be read must not be approved from the phone.
* **Slack, if it is the chat.** The same limit as for any chat: Slack and the hub see the plain text, and the owners of the Slack
  workspace can see what the app's direct message holds (retention, export and e-discovery of the workspace apply). The app has
  the least rights the client uses (`hub slack-manifest` lists them) and reads no channel. Two tokens are secrets: the **bot token**
  posts as the app and reads its direct messages; the **app-level token** opens Socket Mode connections, and an event goes to one
  of the connections that are open, so someone who holds it can receive some of the user's messages and acknowledge them, and the
  hub never sees them. Keep both in the secret store. Reinstalling the app issues a new bot token; the app-level token is made again
  under *Basic Information*, *App-Level Tokens*. Look at the app's connections
  (api.slack.com/apps) if messages go missing. An event is acknowledged to Slack when it arrives, so a hub that stops
  right then loses it; the user sends it again. After a gap in the socket the client reads the history of the direct message back
  from the last message it heard of, through the same checks, and a restarted hub reads nothing back on purpose (a command must
  not run twice).
* **A compromised agent machine** can do what the agent can.
* **Another process of the same user on the agent machine.** It can read the agent's identity, change the lock file, type
  into the tmux session itself and read the spool. Files are mode 0600 and the state directory 0700, which keeps other
  users out, not the same user. Run the agent under an account that runs nothing you do not trust.
* **The permission mode of the assistant.** Whoever controls the chat can press Shift+Tab (`/key btab`) and so cycle the
  assistant through its permission modes, up to one that asks for nothing (auto, and bypass if the session was started
  allowing it); it also answers permission prompts with `/key 1`. This adds no authority over what the chat could already
  do, but it does remove the prompts that you would otherwise see on your phone. The key only cycles and does not report
  the mode it reached. A first entry into auto mode may show a confirmation dialog that the chat must answer.
* **The content of a file is read by the assistant,** so a file can carry instructions for it (prompt injection); files
  and screens that go to the chat are in plaintext for the hub and for Telegram. The denylist of `agent send-file` is a guard
  against accidents, such as being told to send "everything in this folder", not against someone who renames a file.
  It refuses secrets by name (compared without regard to case and Unicode variants, so `.awſ`, `.AWS` and `STATE` are the
  same names as `.aws` and `state`), by content (private keys including PGP blocks, cloud and chat tokens, the bridge's key
  file), and by identity: the file is opened first and compared with `os.SameFile` against the identity file, the alarm
  secret and the files in the state directory (which catches links and hard links), and its ancestors against the state
  directory, the inbox and the identity directory. The decision is taken on the open descriptor's real path
  (`/proc/self/fd` on Linux, `F_GETPATH` on macOS), which closes the swap of a parent directory between check and open; on
  other systems the check by path stays. Files in the chat inbox are not sent back. What remains is a short window in which
  a process that can rename the state or inbox directory could fool the ancestor comparison, and filesystems that fold case
  fully (`ß` and `ss`), which the text check does not cover.
* **The hash of a prompt in a button** covers the dialog as drawn, including a value that the text shows as `<hidden>`.
  It is a keyed hash (HMAC-SHA256 with 32 random bytes in `<state dir>/prompt-key`, mode 0600, created atomically and
  shared by the agent and `agent prompt` through `PB_STATE_DIR`), so what is shown next to the hidden text does not let
  anyone test a guess at the secret. If the key cannot be read, the hook sends a notice without buttons, and the daemon
  logs a warning and runs unkeyed, which makes it refuse the keyed buttons of a hook. A changed pane width changes the
  hash: a valid tap is then refused, and the refusal says to use `/screen`.
* **Hiding secrets** in `/screen` and in prompts is best effort, not a guarantee. `Redact` hides values after token,
  secret, password, passphrase, api key, authorization, cookie and bearer, credentials in URLs, the password flags of
  `curl`, `mysql`, `sshpass`, `* login` and `openssl`, PEM blocks and tokens of well-known shapes. A value ends at white
  space or shell syntax (`; | & < > ( ) $` and the backtick), so the command after it stays visible. Known gaps: a keyword
  followed by a space hides the next word, and a secret that contains `$` is hidden only up to it.
* **Buttons are time-limited.** The hub refuses a tap on a message older than 15 minutes, or one it does not remember (after
  a restart).
* **Reading the pane is reading a user interface.** Which prompt is shown, which mode, whether Claude Code or a shell is in
  the foreground: all of it comes from how tmux names a program and how Claude Code draws its screen, which can change.
  The code fails toward doing nothing (no buttons, no typing, no restart), and a key is pressed only when the hash matches.
  A few milliseconds remain between the agent's look at the screen and the key press, which nothing can close.
* **Availability.** Anyone who can publish to the bus can fill it with junk. The receiver bounds what it holds (32
  messages and 16 MiB in flight) and spends a key agreement on at most 20 new peer keys in a burst, then 5 a second, so
  junk cannot burn its CPU; but it costs bus quota, and while the budget is empty a real new-key message can be refused.
  The budget is not raised for honest traffic: senders of files stay under it by sending in paced batches under one key,
  and a message refused for the budget is logged with a count, at most once per 10 s, so a flood cannot fill the log. Use
  IAM so that only the right service accounts can publish.
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
7. Text is typed into tmux literally after `--`, limited to 8 KiB, with control characters refused; keys come from an
   allow-list. One of them, `BTab` (Shift+Tab, asked for as `/key btab`), cycles Claude Code's permission mode, which
   decides what it may do without asking. The hub and the agent log every use of it; see "What is not protected" for
   what it means.
8. Logs record event names, ids and sizes, never content.
9. Locks fail closed. A lock file that exists, or whose state cannot be checked, means locked, on the agent and on the
   hub alike; a lockdown that cannot be written to disk still holds in memory and the hub is told so. The alarm's state
   file is written before the alarm changes state, and disarming removes it first, so a failure leaves the alarm armed,
   and an alarm whose state file cannot be checked starts armed. A lockdown that did not reach an agent is never
   reported as sent: the hub names the agents it could not reach and repeats the lockdown while they report themselves
   unlocked.
10. A command's lifetime starts when the user typed it (the chat's message date), not when the hub got round to it. A
    backlog from a chat outage is not acted on, except `/lockdown`.
11. A used one-time code stays used across a restart of the agent, and so does a lockout. Lockouts that follow each
    other without a valid code in between double in length, from 5 minutes up to 24 hours.
12. With several agents the hub never guesses where a command goes. Until the user chooses an agent (or names one with
    `@name`, or answers one of its messages) nothing is sent, and a restart of the hub forgets the choice. A tap on an agent
    button counts only from the configured chat and user. The same holds for a swipe-reply to a message the hub no longer
    remembers (after a restart, or when more than 512 newer messages pushed it out): nothing is sent, and the hub asks
    for `@name` or `/agent`.
13. A party that has received nothing from its peer yet holds no receiving chain. The agent, which starts the ratchet,
    refuses every message under the hub's first ratchet key (the one in the plaintext ack) until the hub has answered
    with a key of its own: a chain key of all zeros would be known to anyone who saw the ack.
14. tmux is addressed by the exact session name (`=NAME:`), never by a prefix, so that a closed session cannot send
    text or an answer to a permission prompt into another session whose name starts the same way.
15. A button is a request, not an authority. A tap carries the hash of the prompt it was made for, and the agent presses the
    key only if the screen shows a prompt with that hash and the key is Escape or one of its choices; a key from a
    navigation button has no hash, so Enter and the digits from it are refused while a prompt is on the screen. The hub
    accepts a tap only from the configured chat and user, for a known and online agent, for the keys Enter, Escape, Up,
    Down, Tab and 1 to 9 (never `BTab`), while it is not locked, on a message it sent less than 15 minutes ago; a message
    loses its buttons after one tap, and keeps them if the tap could not be sent.
16. Files from the machine are refused by the agent before anything leaves it: by name, by place, by content, by identity
    and under every link that leads there, at most 10 a minute, none while the machine is locked. The hub treats every part
    and closing item as hostile: strict parsing of its metadata, at most 16 live transfers and 2 finished files in memory
    (their bytes are freed as soon as a file is finished; no timer holds them), size, count and hash checked before
    anything is uploaded, one upload at a time, every record with a bounded life. A give-up is confirmed to the agent only
    after the chat was told, so a file is never lost without a word.
17. Files from the chat are bounded in size (Telegram's limit, the hub's limit, the download stopped one byte over), in
    count (2 transfers on the hub; the agent's assembler holds 16 and drops idle ones), in memory and in time. Names are
    reduced to harmless characters and placed under the inbox only; the inbox is mode 0700 and its files 0600, created
    without ever overwriting or following a link. Tidying removes only the bridge's own files. The caption is one line, cut
    to 500 characters, with control and bidirectional characters removed. A locked machine takes no part and no file.
    Nothing from a file is ever executed or interpreted by the bridge. The download address contains the bot token and is
    never followed to another host or logged. Commands typed after a file wait behind it (at most 32 per agent) and are
    discarded by `/lockdown`, so nothing typed before a lockdown is sent after it.
18. What the bridge starts or types on the machine on its own (auto-heal, `/continue`) comes only from the agent's local
    settings (`PB_ASSISTANT_COMMAND`, `PB_WORKDIR`); nothing from the chat reaches a shell. Both are refused on a locked
    machine, `/continue` expires like a key, and the agent never types into a pane that looks like Claude Code's own screen.
19. Text from the chat is not typed into a shell or into a dialog. If the foreground program of the active pane is a shell
    and the screen is not Claude Code's, the text is refused instead of being run as a command. A screen counts as Claude
    Code's only by its end: its markers must be in the last lines and the last line must not be a shell prompt, so the
    remains of an earlier session above a prompt do not pass. Text, and the typed line about a file, are also refused while
    a dialog with numbered choices is on the screen (an Enter would select the highlighted choice). Each typing invocation
    first takes the pane out of copy mode, in the same tmux invocation. Auto-heal and `/continue` type into a shell only when
    the prompt is idle and unchanged for the whole grace period.

## Tests that correspond to attacks

| Attack | Tests |
|---|---|
| Forged packets, fresh-key headers, state corruption | `ratchet.TestFreshInitiatorRejectsMessageUnderTheResponderFirstKey`, `ratchet.TestForgeriesNeverBreakTheSession`, `TestLowOrderKeyInHeaderIsRejected`, `FuzzDecrypt`, `e2e.TestForgedFloodNeverBreaksTheSession`, `e2e.TestEmulatorReplayAndInjectionAreHarmless` |
| Any altered byte, wrong direction or session | `ratchet.TestEveryByteIsAuthenticated`, `TestWrongAssociatedDataIsRejected`, `wire.TestMessageCannotBeMovedToAnotherDirectionOrSession`, `wire.TestDirectionIsEnforced` |
| Replay of messages | `ratchet.TestReplayIsRejected`, `e2e.TestReplayOfEveryCapturedMessageIsHarmless`, `e2e.TestReplayOfAgentMessagesToTheHub` |
| Replay or forgery of handshakes | `handshake.TestEveryFieldIsSigned`, `TestReplayedHelloIsRejected`, `TestGuardOnlyRecordsAuthenticatedHellos`, `TestAckMustAnswerOurHello`, `TestAckFromAnImpostorIsRejected`, `TestKindConfusionAndReflection`, `TestStaleAndFutureHellosAreRejected`, `e2e.TestReplayedRealHelloCannotResetTheSession`, `e2e.TestForgedHellosAreRefusedAndTheSessionSurvives` |
| Stolen signing key, degenerate keys, the KEM | `handshake.TestStolenSigningKeyAloneDoesNotGiveTheSession`, `TestDegenerateEphemeralKeyIsRejected`, `TestKEMSecretContributesToTheKey` |
| Resource exhaustion | `ratchet.TestSkipLimit`, `TestStoredSkippedKeysAreBounded`, `TestAFloodOfFreshKeysIsCutOffBeforeItCostsAnything`, `TestARealKeyStepWaitsForTheBudgetAndThenWorks`, `handshake.TestGuardIsBoundedAndForgets`, `TestOneSenderCannotFillTheGuardForTheOthers`, `TestVerifyHelloRecordsNothing`, `spool.TestSizeAndCountLimits`, `TestOversizedAndCorruptFilesAreSetAside`, `agentsvc.TestTheQueueCannotBeMadeToGrowWithoutLimit`, `hubsvc.TestAStrangersMessagesAreCountedButDoNotFloodTheLog` |
| Forward secrecy and healing | `ratchet.TestForwardSecrecy`, `TestPostCompromiseSecurity`, `TestRestoringAnOldStateBreaksTheSession` |
| Stale, reordered, lost and late commands | `e2e.TestDelayedCommandsExpireInsteadOfRunning`, `TestCommandsRunInTheOrderTheyWereTyped`, `TestKeysAndTextShareOneOrder`, `TestALostCommandIsReportedAndTheNextOnesStillRun`, `TestACommandThatArrivesAfterItsTurnIsNotRun`, `TestOldChatMessagesAreNotActedOn`, `TestLockdownIsObeyedWhateverItsAge`, `TestAnOldUnlockIsIgnored`, `TestEmulatorRapidCommandsRunInOrder`, `agentsvc.TestCommandsRunInTheHubsOrderWhateverTheArrivalOrder`, `TestAGapHoldsLaterCommandsForTheWindowAndThenIsReported`, `TestACommandWithoutANumberIsRefused`, `TestExpiryIsCheckedWhenACommandIsAboutToRunNotWhenItArrived` |
| Unauthorised chat sender | `e2e.TestOnlyTheAuthorisedUserIsObeyed` |
| One agent acting on another's messages | `e2e.TestAgentsCannotReadOrActForEachOther`, `TestAWrongSessionCommandIsRefused` |
| Lockdown and local unlock | `hubsvc.TestALockdownThatDidNotGetThroughIsNotReportedAsSentAndIsRepeated`, `hubsvc.TestAHubLockFileThatCannotBeCheckedMeansLocked`, `agentsvc.TestALockdownIsNotUndoneByAConcurrentCheckWhenTheFileCannotBeWritten`, `e2e.TestLockdownIsEasyToStartAndNeedsALocalUnlock`, `TestLockSurvivesAnAgentRestart`, `TestHubLockdownStopsRelayingEvenToAnUnlockedAgent`, `TestALockThatCannotBeWrittenStillHoldsAndIsReported`, `TestALockWhoseFileCannotBeCheckedFailsClosed`, `TestAHubLockdownIsRepeatedToAnAgentThatReconnects`, `agentsvc.TestLockIsCheckedWhenACommandIsAboutToRunNotWhenItArrived` |
| Session setup races | `agentsvc.TestConcurrentAcksGiveOneSessionAndItIsTheOneReadyIsSentFor`, `hubsvc.TestAHelloWhoseAckCouldNotBePublishedIsAnsweredWhenTheBusDeliversItAgain`, `e2e.TestSeveralHellosInFlightStillGiveOneWorkingSession`, `TestReadyIsRepeatedWhenTheFirstOneIsLost` |
| Silencing the alarm | `approval.TestEachConsecutiveLockoutIsTwiceAsLong`, `alarm.TestAnAlarmWhoseStateFileCannotBeCheckedStartsArmed`, `e2e.TestDisarmingCanRequireAOneTimeCode`, `TestAReplayedDisarmCommandDoesNotSilenceAReArmedAlarm`, `TestADelayedDisarmCommandExpires`, `TestALockedMachineKeepsItsAlarmArmed`, `approval.TestACodeWorksOnlyOnce`, `TestLockoutAfterRepeatedFailures`, `TestAUsedCodeStaysUsedAfterARestart`, `TestALockoutSurvivesARestart`, `TestACodeWhoseUseCannotBeRecordedIsRefused`, `alarm.TestArmingTakesTheBaselineOnlyWhenItWasOff`, `TestArmFailsCleanlyWhenTheStateFileCannotBeWritten`, `TestDisarmFailsSafeWhenTheStateFileCannotBeRemoved` |
| Commands after a file, pacing, reorder | `hubsvc` order tests in `order_test.go` (photo then text, two photos, failing transfer, lockdown drops the held commands), `e2e.TestCommandsWaitingWhenTheSessionIsReplacedAreReportedAsDropped`, `hubsvc.TestTheAgentsReportOfACommandTheHubCouldNotPublishIsNotNews` |
| Lost replies and alerts | `e2e.TestSpoolSurvivesAHubOutageAndIsDeliveredLater`, `TestAnAlertSurvivesAHubOutage`, `TestAnAlertSurvivesAChatOutage`, `e2e.TestEmulatorReplyWrittenDuringAHubOutageIsDelivered` |
| Stale or forged button taps | `agentsvc.TestAKeyForAnEarlierPromptIsNotPressedForALaterOne`, `TestEnterFromAnOldSnapshotDoesNotApproveAPromptThatCameLater`, `TestOnlyEscapeAndTheNumbersOfTheChoicesAnswerAPrompt`, `hubsvc.TestATapOnAnOldNoticeDoesNotAnswerANewPrompt`, `TestTheButtonsOfAMessageExpireAfterFifteenMinutes`, `TestATapOnAMessageTheHubDoesNotRememberIsRefused`, `TestATapThatSentNothingKeepsTheButtonsAndSaysSo`, `TestNoButtonCanPressBTab`, `agentsvc.TestAButtonMadeWithAnotherKeyIsRefused`, `screen.TestANumberedListInTheCommandDoesNotShadowTheChoices`, `TestTheHashSeesIndentationBlankLinesAndTipLinesOfTheCommand`, `TestLoadKeyGivesEveryCallerThatRacesTheSameKey`, `TestRedactHidesTheCommonFormsOfSecrets`, `TestAHiddenValueEndsAtShellSyntax`, `promptcmd.TestATooLongCommandOffersOnlyEscAndSaysWhy`, `e2e` tests on a real tmux pane in `ui_tmux_test.go` |
| Hostile files, names and sizes | `filetx` tests (`TestSecretsAndTheirPlacesAreRefused`, `TestAStrangerCannotMakeTheReceiverHoldMoreThanTheBounds`), `smoke.TestSendFile*` (the real binary), `filetx.TestNamesThatAFileSystemFoldsToASecretAreRefused`, `cmd/agent` `TestADirectorySwappedForALinkBetweenTheCheckAndTheOpenIsNotReadThrough`, `TestAHardLinkToAFileOfTheBridgeIsRefused`, `TestFilesInTheInboxAreNotSentBack`, `hubsvc.TestThePartsGoOutInBatchesOfAWindowOfSixteenAndTheNextBatchWaitsForHalfOfItToBeFree`, `TestAGiveUpIsConfirmedOnlyWhenTheChatHasHeardIt`, `e2e` pacing tests in `pace_test.go` (45 MiB each way at 0, 10 and 25 ms, both ways at once), `agentsvc` inbox tests (links, `O_EXCL`, hostile names and captions), `telegram.TestFetchDoesNotFollowARedirectToAnotherHost` and `TestFetchErrorsNeverCarryTheToken` |
| Starting and typing by itself | `agentsvc` heal tests (grace, tries, lock, shell names, panes that look like Claude Code), `TestOnlyTheEndOfTheScreenMakesItClaudeCode`, `TestARemnantOfClaudeCodeAboveAShellPromptDoesNotLetTextIntoTheShell`, `TestNoTextIsTypedWhileAPromptIsOnTheScreen`, `TestTheLineAboutAFileIsNotTypedIntoAPromptOrAShellButTheFileIsSaved`, `TestAHalfTypedLineAtTheShellIsNeverGluedToTheCommand`, `TestHealStartChecksAgainUnderTheLockWhatTheTickDecidedOn`, `e2e.TestTextIsNotTypedIntoAShellThatReplacedClaudeCode`, `TestNoTextIsTypedWhileAPromptWaitsForAnAnswer` |
| Input into tmux | `tmuxctl.TestRealTmuxDoesNotTypeIntoASessionWhoseNameOnlyStartsWithOurs`, `TestRealTmuxFinishesTheSequenceWhenTheContextEndsDuringThePause`, `tmuxctl.TestSendTextIsLiteralAndSeparatedByDoubleDash`, `TestBadTextIsRefusedBeforeTmuxRuns`, `TestKeyAllowList`, and against the real tmux `TestRealTmuxTypesTheTextExactly` (a trailing `;` or `\;`, option-like text, shell syntax, unicode), `TestRealTmuxDoesNotInterleaveConcurrentSenders`, `TestRealTmuxPaneCommandIsThatOfTheActivePaneOfASplitWindow`, `TestRealTmuxTextAndKeysReachAPaneThatIsInCopyMode` |
| Secrets in logs and errors | `telegram.TestTheTokenNeverAppearsInErrors`, `TestAStalledConnectionDoesNotHangTheHub`, `smoke.TestRealBinariesEndToEnd` (checks the real binaries' logs), `config.TestTheTokenIsNeverAFlag`, `TestBotAPIAddressMustNotSendTheTokenInTheClear`, `TestGroupReadableSecretsNeedTheExplicitSetting`, `identity.TestLoadRefusesWorldReadableFile`, `TestGroupReadIsAllowedOnlyWhenAskedFor` |

## Reporting a problem

Please report security problems privately to the maintainer before publishing them.
