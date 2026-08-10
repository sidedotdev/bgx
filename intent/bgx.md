---
intent_links:
  - intent: "#bgx"
    code:
      - main.go
      - cmd/bgx/main.go
      - daemon/daemon.go
      - daemon/attach.go:serveAttach
      - scrollback/store.go
      - scrollback/store.go:Snapshot
      - scrollback/backend.go:Config
      - vtscan/vtscan.go
      - vt/vt.go
      - client.go
      - transport.go:Bridge
  - intent: "#commands"
    code:
      - internal/cli/cli.go:runner.rootCommand
      - internal/cli/cli.go:runner.runAction
      - internal/cli/cli.go:runner.waitAction
      - internal/cli/cli.go:runner.killAction
      - internal/cli/cli.go:runner.historyAction
      - internal/cli/cli.go:runner.sendAction
      - internal/cli/cli.go:runner.infoAction
      - internal/cli/cli.go:runner.listAction
      - internal/cli/cli.go:runner.attachCommand
      - internal/cli/cli.go:runner.attachAction
      - internal/cli/cli.go:runner.bridgeCommand
      - internal/cli/cli.go:runner.bridgeAction
      - attach.go:transportConn
      - transport.go:Bridge
      - client_attach.go:runTerminalAttach
      - client_attach_test.go:TestClientAttachReportsMalformedDaemonFrame
      - client_attach_test.go:TestClientAttachDisconnectClearsDetachInstructionsWithoutReset
      - e2e/attach_errors_test.go:TestAttachDisconnectClearsDetachInstructionsWithoutReset
      - e2e/bridge_test.go:TestBridgeForwardsAttachProtocolVerbatim
      - e2e/bridge_test.go:TestAttachViaTransportBridgesRemoteSession
      - e2e/bridge_test.go:TestBridgeReportsEndedAndMissingSessions
      - e2e/bridge_test.go:TestAttachRejectsBothSSHAndVia
      - e2e/bridge_test.go:TestAttachSSHExpandsToTransportCommand
      - e2e/bridge_test.go:TestAttachViaReportsRemoteMissingAndEndedSessions
      - internal/cli/cli_test.go:TestRootCommandsHaveLibraryOperations
      - e2e/attach_test.go:TestAttachReportsEndedAndMissingSessions
      - attachview.go:attachView
      - e2e/attach_test.go:TestAttachShowDetachInstructionsReservesLine
      - e2e/attach_test.go:TestAttachShowDetachInstructionsOneRowTerminal
      - e2e/attach_test.go:TestAttachShowDetachInstructionsSurvivesDestructiveOutput
      - internal/cli/cli.go:runner.versionAction
      - command_api.go:Version
  - intent: "#constraints"
    code:
      - client.go:failJSONCode
      - client.go:waitForSession
      - client.go:spawnDaemon
      - client.go:startupError
      - client.go:recheckEndedRecord
      - e2e/run_test.go:TestRunFailsPromptlyWhenDaemonExitsBeforeStartup
      - e2e/run_test.go:FuzzRunShortLivedSessionExitCode
  - intent: "#filesystem-requirements"
    code:
      - dirs.go
      - dirs.go:resolveDirs
      - dirs.go:resolveStateDir
      - dirs.go:dirCandidates
      - dirs.go:stateDirCandidates
      - dirs.go:computeDirCandidates
      - dirs.go:socketDir
      - dirs.go:retentionDir
      - dirs.go:ensureDirs
      - dirs.go:fallbackNotice
      - dirs_test.go:TestComputeDirCandidatesReportsConfiguredXDGAccessError
      - internal/cli/cli.go:runner.withDirs
      - internal/cli/cli.go:runner.runAction
      - internal/cli/cli.go:runner.versionAction
      - command_api.go:Version
      - transport_test.go:TestMain
      - e2e/run_test.go:bgxIn
      - e2e/attach_test.go
      - e2e/bridge_test.go
      - e2e/filesystem_test.go
      - e2e/filesystem_test.go:TestStateHomeStoresHistoryData
  - intent: "#testing--verification"
    code:
      - e2e/main_test.go:run
      - e2e/run_test.go
      - e2e/kill_send_history_test.go
      - e2e/attach_test.go
      - e2e/bridge_test.go
      - e2e/list_test.go
      - e2e/version_test.go
      - e2e/filesystem_test.go
  - intent: "#implementation"
    code:
      - go.mod
      - go.sum
      - cmd/bgx/main.go
      - daemonize.go:InterceptDaemon
      - daemon/daemon.go:Serve
      - daemon/daemon.go:pumpOutput
      - daemon/protocol.go
      - daemon/frame.go
      - scrollback/backend.go
      - scrollback/store.go:Snapshot
      - vtscan/vtscan.go
      - vt/vt.go
      - vt/vt_test.go
      - e2e/attach_test.go
---

# bgx

bgx is a terminal session management cli tool similar to screen/tmux, but
designed for async programmatic use. It is inspired by
(zmx)[https://github.com/neurosnap/zmx], generally copying its approach and
sharing with it the following features:

1. separate client and daemon(s)
1. daemon per session
1. allow multiple clients to concurrently attach to the same session
1. overlapping [sub-commands](#commands), though with a slightly different
   flavor
1. uses the same libghostty-vt approach for rendering latest terminal state

But is customized for our needs:

1. Re-implemented in golang so it's usable both as a library and as a cli tool.
1. The run subcommand is always async.
1. A session is only ever used to execute a single command, sessions can not be
   continued after the command exits.
1. An info subcommand provides metadata about a session such as whether it
   exists, start time, output size, duration, custom metadata and exit code (if
   exited).
1. Stores first ~1mb (head) and last ~9mb (tail), discarding the middle. Note
   that this storage does not include the uncompressed buffer of scrollback.
   Limits are approximate to reflect that the sizes adjust slightly to ensure VT
   is at ground state and not splitting runes. Both approximate history sizes
   are configurable. Storage defaults to memory but can be configured to use
   disk, either in a tmp directory or a custom path.
1. When viewing history, the discarded portion is clearly demarcated, replaced
   with an empty line, then a marker line, followed by a count of how many bytes
   were discarded (format: "[...] truncated 1.5MB" with human-readable units),
   then marker and empty line again. The tail also applies on top of a clear
   terminal state (via a full-reset preamble just before the tail), allowing
   visual fidelity to be largely maintained and even self-heal in most typical
   scenarios of non-TUI command output.
1. Compresses both the head and tail. Uses zstd compression for the unbuffered
   scrollback history, recompressing after buffering every new uncompressed ~1mb
   chunk (across both head and tail). The size of the chunk is slightly adjusted
   to ensure chunk boundaries line up with ground/rune boundaries.
1. Avoids backpressure on the commands in the session, which requires a dynamic
   buffer size. Falls back to temporarily not compressing if the buffer size
   while compressing grows beyond 10mb (configurable), avoiding too much memory
   bloat in the case of very high throughput writes where compression isn't
   keeping up.
1. All commands default to json output, other than history and attach.
1. The daemon retains the history for the last 10 sessions (configurable) that
   were finished/killed/current per id namespace (namespace is the part of
   session id before first "/"), writing these to a designated tmp directory by
   default, or other configured storage.
1. Only allows up to 3 (configurable) concurrent sessions that are still active
   per id namespace. Fails with a clear error, listing all session info, when
   attempting to run one over the threshold.
1. Sessions can be tagged with an arbitrary map of metadata when created, which
   is included when sessions are listed, or used as a filter directly against
   top-level metadata keys in the list subcommand.
1. Supports bridging to remote sessions

## Commands

- run <id> [--overwrite-id] [--metadata key=value...] <command...>
   - Run command async in new session. Session ends when command exits or is
     explicitly killed
   - If id already exists on an ended session, fails unless --overwrite-id is
     passed
   - Quotes not needed for command
   - Interactive prompts will *not* hang as long as someone attaches to the
     session to unblock it or sends input to it via the send subcommand
   - To run a script, callers may use something like `bash -c '...'`
- wait <id>
   - Wait for session to finish. Returns exit code.
- kill <id>
   - Syncronously kills the session
   - Ensures all output up to the point the session is killed is retained
   - Any still-attached clients will also receive all such output before closing
     automatically
- history <id>
- attach [--ssh <host>] <id> [--show-detach-instructions] [--via <cmd...>]
   - When initiated, detects if the session has already ended or doesn't exist,
     outputting the appropriate error for each case
   - Attaches to session, outputs "current" rendered terminal state
     (specifically: the most recent available ground and rune boundary state)
     and continues to update it by streaming raw output since that state
   - Clients that cannot keep up with consuming the stream (resulting in full
     buffers) result in the attached client gracefully skipping forward by
     re-attaching at a later point, getting the latest rendered terminal state
     and resuming from that point.
   - Writes all concurrently connecting clients' input to the session PTY
   - Client resizes are forwarded to the session PTY. When there are multiple,
     the smallest column size, and smallest row size across all clients (even if
     different clients) is forwarded, ensuring non-broken rendering
   - Detach with ctrl+\, which resets the terminal to state prior to attaching
   - Closes automatically when session ends or disconnects, resetting the cursor
     but not the entire terminal state
   - Showing detach instructions means a line of the terminal is reserved by the
     client for showing how to detach.
     - This line is cleared when session ends or disconnects.
     - The instruction line is styled with a separate subtle background color.
    - `--via` is the unsugared form of `--ssh`, allowing more arbitrary commands
      to be used. providing both is an error.
  - bridge <id>`
    - like `attach` except forwards the raw underlying frames, which allows a
      remote client to attach to the underlying session through `bgx attach
      --via ssh user@host`

- send <id> <text...>
   - Send raw input to session PTY without attaching
- list|ls
- version
- help

## Additional Requirements

The following are requirements that are not covered in the (overview)[#bgx].

### High-Level Objectives

- "Just Works"
  - Works without special configuration in arbitrary environments on all
    supported platforms, via best-effort fallback logic

### High-Level Constraints

- A daemon must outlive the client that starts it
- Does not require a specific filesystem structure

### Error Requirements

- Errors always return json, even for commands that normally do not, with unique
  error codes included alongside the error message
- Errors are always in stderr, not stdout
- Errors include a `"source"` key set to `"bgx"` as an affordance

### `run` Requirements

- If it exits before the session becomes available, run must fail promptly with
an error including any daemon stderr output.

### Filesystem Requirements

- bgx respects $XDG_RUNTIME_DIR for sockets
- bgx respects $XDG_STATE_HOME for history data
- bgx clearly reports errors with accessing XDG directories when it is set
- If not set, or not accessible, bgx prefers default XDG directories when
  possible, falling back to other options as needed: $HOME/.bgx/, then /tmp/bgx/
  (and/or others as appropriate per platform), and then ./.bgx/ as a last resort
  only
- bgx creates fallback directories idempotently, only when needed
- bgx moves to the next fallback if creation of a fallback directory fails or
  reading/writing to it fails due to permissions
- bgx logs to stderr and includes similar metadata in json outputs upon fallback
- bgx fails and reports a clear error when all fallbacks fail

## Testing / Verification

- End-to-end tests verify the observable behavior of each of the CLI tool's
subcommands in a black-box manner.
- The [#high-level-constraints] each have associated tests strongly validating the
  constraint is  met
- The [#filesystem-requirements] are strongly validated via blackbox tests
  - These tests use bubblewrap or seatbelt to simulate access issues, and docker to simulate missing directories.
- Logic that can be affected by timing is validated through fuzz testing that
exercises all potential scenarios to discover race conditions automatically

## Implementation

Uses adrg/xdg, urfave/cli/v3 and ehsanul/libghostty-vt-static. Other
dependencies are limited to those required for robustness/correctness and to
support multiple platforms effectively.
