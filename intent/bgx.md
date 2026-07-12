---
intent_links:
  - intent: "#bgx"
    code:
      - main.go
      - daemon/daemon.go
      - daemon/attach.go:serveAttach
      - scrollback/store.go
      - scrollback/store.go:Snapshot
      - scrollback/backend.go:Config
      - vtscan/vtscan.go
      - vt/vt.go
      - client.go
  - intent: "#commands"
    code:
      - main.go:newApp
      - client.go:runAction
      - client.go:waitAction
      - client.go:killAction
      - client.go:historyAction
      - client.go:sendAction
      - client.go:infoAction
      - client.go:listAction
      - attach.go:attachAction
      - main.go:versionAction
  - intent: "#constraints"
    code:
      - client.go:waitForSession
      - client.go:spawnDaemon
      - client.go:startupError
      - client.go:recheckEndedRecord
      - e2e/run_test.go:TestRunFailsPromptlyWhenDaemonExitsBeforeStartup
      - e2e/run_test.go:FuzzRunShortLivedSessionExitCode
  - intent: "#testing--verification"
    code:
      - e2e/run_test.go
      - e2e/kill_send_history_test.go
      - e2e/attach_test.go
      - e2e/list_test.go
      - e2e/version_test.go
  - intent: "#implementation"
    code:
      - main.go:daemonCommand
      - daemon/daemon.go:Serve
      - daemon/daemon.go:pumpOutput
      - daemon/protocol.go
      - daemon/frame.go
      - scrollback/backend.go
      - scrollback/store.go:Snapshot
      - vtscan/vtscan.go
      - vt/vt.go
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
- attach <id>
   - Session must exist and be running
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
   - Detach with ctrl+\
   - Closes automatically when session ends, resetting the cursor but not the
     entire terminal state
- send <id> <text...>
   - Send raw input to session PTY without attaching
- list|ls
- version
- help

## Constraints

- A daemon must outlive the client that starts it.
- If it exits before the session becomes available, run must fail promptly with
  an error including any daemon stderr output

Address review comments on constraints commit:

<!-- I have a few review notes before preferring it over the pushed client fix:

It still discards daemon stderr. For SIGILL, Wait() is probably sufficient, but
ordinary startup errors that print a useful explanation and exit 1 become only
exit status 1. My implementation retained the first stderr line using an
independent temporary file, so it handles both crashes and normal initialization
failures. If the intended contract is merely “not a timeout,” this version is
enough; if errors must be actionable, preserving stderr is stronger.

The test should assert the new behavior, not just any JSON error. I would
require the error to contain daemon exited before startup completed and not
contain timed out. Its timing assertion proves promptness, but checking the
message prevents an unrelated early validation failure from satisfying the test.

Immediate record recheck has a small race. After Wait() wins, a very short-lived
valid session’s ended record may be in the process of becoming observable. A
short bounded retry for the record is safer. The existing
TestRunWaitReturnsNonzeroExitCode is important to run repeatedly because it
exercises this boundary.

-->

## Testing / Verification

- End-to-end tests verify the observable behavior of each of the CLI tool's
subcommands in a black-box manner.
- The [#constraints] each have  associated tests strongly validating the constraint is met
- Logic that can be affected by timing is validated through fuzz testing that exercises all potential scenarios to discover race conditions automatically

## Implementation

Uses adrg/xdg, urfave/cli/v3 and libghostty-vt (or go bindings to it). Other
dependencies are limited to those required for robustness/correctness and to
support multiple platforms effectively.
