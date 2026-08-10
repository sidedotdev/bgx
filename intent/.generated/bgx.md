---
intent_links:
  - intent: "#daemonization"
    code:
      - daemonize.go:InterceptDaemon
      - client.go:spawnDaemon
      - cmd/bgx/main.go
      - daemon/daemon.go:persistSpawnError
  - intent: "#socket-protocol"
    code:
      - daemon/protocol.go
      - daemon/frame.go
      - client.go:dialRequest
      - daemon/attach.go
      - e2e/bridge_test.go:TestBridgeForwardsAttachProtocolVerbatim
  - intent: "#terminal-state"
    code:
      - vt/vt.go:Terminal
      - daemon/daemon.go:Session
  - intent: "#attach-handoff"
    code:
      - attach.go:Attach
      - attach.go:startTransportProcess
      - attach.go:transportConn.Close
      - daemon/attach.go:serveAttach
      - daemon/daemon.go:pumpOutput
      - daemon/daemon_test.go:TestAttachSnapshotStreamCoversEntireOutput
      - client_attach.go:runTerminalAttach
      - client_attach_test.go:TestClientAttachSkipsUnavailableSizeUntilValidResize
      - client_attach_test.go:TestClientAttachReportsActionableSizeError
      - client_attach_test.go:TestClientAttachWaitsForResizeWorkerBeforeRestore
      - client_attach_test.go:TestClientAttachWaitsForBlockedInputBeforeRestore
      - client_attach_test.go:TestClientAttachReportsTerminalWriteFailure
      - client_attach_test.go:TestClientAttachReportsTerminalRestoreFailure
      - client_attach_test.go:TestClientAttachReportsInitialResizeFailure
      - client_attach_test.go:TestClientAttachReportsMalformedDaemonFrame
      - client_attach_test.go:TestClientAttachDetachIsSuccessful
      - attachview.go:attachView
      - attachview.go:paint
      - e2e/attach_test.go:TestAttachShowDetachInstructionsSurvivesDestructiveOutput
  - intent: "#send-and-wait-semantics"
    code:
      - client.go:sendAction
      - client.go:waitAction
      - client.go:emitExit
  - intent: "#configuration-and-layout"
    code:
      - dirs.go:socketDir
      - dirs.go:retentionDir
      - client.go:socketPath
      - client.go:runningInNamespace
      - client.go:failConcurrencyLimit
      - daemon/retention.go:Namespace
      - daemon/retention.go:activeNamespaceSessions
      - daemon/daemon.go:persist
  - intent: "#base-directory-resolution"
    code:
      - dirs.go
      - dirs.go:resolveDirs
      - dirs.go:resolveStateDir
      - dirs.go:computeDirs
      - dirs.go:computeDirResolution
      - dirs.go:dirCandidates
      - dirs.go:stateDirCandidates
      - dirs.go:usableDir
      - dirs.go:ensureDirs
      - dirs.go:fallbackNotice
      - transport_test.go:TestMain
      - e2e/run_test.go:bgxIn
      - e2e/attach_test.go
      - e2e/bridge_test.go
      - e2e/filesystem_test.go:TestStateHomeStoresHistoryData
      - internal/cli/cli.go:runner.withDirs
  - intent: "#error-reporting"
    code:
      - errors.go
      - internal/cli/cli.go:runner.run
      - cmd/bgx/main.go:main
      - client.go:failConcurrencyLimit
      - e2e/errors_test.go
  - intent: "#boundary-alignment-and-truncation-demarcation"
    code:
      - vtscan/vtscan.go
      - scrollback/store.go
      - scrollback/store.go:Snapshot
      - scrollback/backend.go:Config
      - daemon/daemon.go:feedTerm
      - daemon/daemon.go:pumpOutput
      - daemon/attach.go:serveAttach
---

# bgx (generated)

Concise record of consequential decisions inferred while implementing
`intent/bgx.md`. The human-authored intent remains the source of truth; this
file captures design choices not spelled out there.

## Daemonization

Each session runs in its own detached daemon process, created by re-exec'ing the
bgx binary's hidden `__daemon` subcommand with `setsid` and detached stdio so it
outlives the spawning client. The daemon owns the PTY, scrollback store, and
unix socket for the session and exits once the command ends and its record is
persisted.

Because the daemon's stderr is detached to `/dev/null`, a wrapped command that
fails to exec would otherwise die silently and leave `run`/`info` only able to
report a socket-readiness timeout. To keep the cause visible, the daemon
persists an ended record with a non-empty `Error` and a conventional 127 exit
code when the command never starts; `run` then fails with that error instead of
timing out.

## Socket protocol

One unix domain socket per session lives under the XDG runtime dir (tmp
fallback), with the session id encoded into a single safe filename component.
Clients speak a JSON-line request/response protocol (`info`, `wait`, `kill`,
`send`, `history`). `attach` upgrades the same connection, after the JSON
handshake, to tagged length-prefixed binary frames
(Input/Output/Resize/Detach/Ended) for raw bidirectional bridging. Ended carries
no payload and is followed by connection closure. `history` is returned as
base64 in the JSON response and written raw to stdout by the client.

## Terminal state

The daemon maintains a libghostty-vt terminal fed every PTY output byte
alongside the scrollback store, fixed at an 80x24 default size. Attach uses
`DumpScreen` for the initial snapshot; the attach leader controls PTY and vt
size via Resize frames. The daemon answers Device Attributes queries itself when
no client is attached so interactive programs don't hang.

## Attach handoff

A client joining a live session receives an ordinary Output frame containing
RIS followed by a point-in-time `DumpScreen` rendering, then the raw output
stream. To avoid losing or duplicating output produced between rendering the
snapshot and subscribing to the stream, `serveAttach` captures the snapshot and
joins the output fanout under the same `outMu` that `pumpOutput` holds while
writing each chunk to the terminal and fanning it out. Each PTY chunk therefore
lands entirely before the snapshot (reflected in it, not streamed) or entirely
after the subscription (streamed, not in the snapshot), so a client's snapshot
and stream tile the full session output with no gap or overlap. If a client
falls behind, its queued output is replaced with a newer RIS-prefixed snapshot,
also carried as an ordinary Output frame, before live streaming resumes.
`TestAttachSnapshotStreamCoversEntireOutput` and
`TestSlowClientResyncsInsteadOfDisconnect` guard these invariants.

With `--show-detach-instructions` the client cannot forward session bytes to the
physical terminal, because the stream may clear the screen, reset scrolling
margins, address the cursor absolutely or switch to the alternate screen, any of
which would corrupt the reserved line. Instead the client keeps its own
libghostty-vt terminal sized to cols x (rows-1) — the size it also advertises to
the daemon — feeds every Output frame into it, and paints its `DumpScreen` plus
the hint as a single coalesced redraw. RIS-prefixed snapshots reset this local
terminal in-band. A terminal with only one row reserves nothing and gets the
full size.

## Send and wait semantics

`send` writes exactly the argv joined by single spaces as raw PTY bytes — no
trailing newline and no completion markers; callers send line endings
themselves. `wait` returns the exit code as JSON and additionally exits the bgx
process with that same code.

## Configuration and layout

Scrollback head/tail sizes, storage kind/path, and retention count are
configured via `run` flags (with env fallbacks) and forwarded to the daemon.
Ended-session records and histories are persisted under the XDG state directory
(with home/tmp fallbacks), grouped by id namespace (the substring before the
first "/"; slashless ids share one global namespace), keeping only the newest N
(default 10) per namespace. Currently-running sessions count toward that N: when
an ending session prunes
its namespace, it reserves one slot for each live session sharing the namespace
(discovered by scanning the socket dir), so finished/killed records plus active
sessions together stay within the limit.

`run` also enforces a per-namespace cap on concurrently active sessions
(default 3, set via `--concurrency`/`BGX_CONCURRENCY`). The check and spawn are
serialized under a per-namespace advisory file lock in the socket dir, so
simultaneous `run` invocations cannot race past the cap. When a namespace is
already at its limit, `run` fails with a JSON error that lists every active
session so the caller can act on it, and does not spawn a daemon. Both the cap
and the retention slot accounting count only sockets with a live listener,
ignoring stale socket files left by crashed daemons.

## Error reporting

Every command failure emits a single JSON object on stderr — never stdout —
shaped `{"error": <message>, "code": <code>, "source": "bgx"}` and exits
non-zero. The `source` key is always `"bgx"` so consumers can tell the error
came from bgx itself rather than the wrapped command. Codes form a small stable
taxonomy (`invalid_argument`, `not_found`, `already_exists`,
`concurrency_limit`, `startup_failed`, `filesystem`, `internal`); the
concurrency-limit payload additionally carries the offending `sessions`.
urfave/cli usage errors and unknown commands are intercepted so parse failures
follow the same contract instead of plain-text usage/help output.

## Base directory resolution

bgx independently resolves runtime and state base directories once per process.
Sockets prefer `$XDG_RUNTIME_DIR/bgx`, then the default XDG runtime directory;
ended records and histories prefer `$XDG_STATE_HOME/bgx`, then the default XDG
state directory. Both chains continue through `$HOME/.bgx`, `<tmp>/bgx`, and
`<cwd>/.bgx`. Each candidate is created idempotently (`0700`) and probed for
write access; a candidate that fails to create or write advances to the next.
An explicitly set but unusable XDG directory is called out by name. Fallbacks
are logged to stderr once per chain and combined in JSON output (`run`,
`version`) via a `fallback` field. If either chain has no usable candidate,
client commands report a clear error and do nothing else. Resolution lives only
in the client: the daemon receives concrete socket and retention paths and
never re-resolves.

## Boundary alignment and truncation demarcation

A dependency-free `vtscan` package implements a minimal hand-rolled VT500 parser
plus UTF-8 tracking, exposing whether the parser sits at ground state on a rune
boundary and a `SafeCut` that finds the largest safe offset at or before a
target size. Both the scrollback store and the daemon import it (it stays
cgo-free unlike `vt`).

The scrollback store compresses head and tail through one chunk pipeline and
uses `vtscan` to nudge chunk-flush points, the head/tail split, and tail trims
onto ground/rune boundaries, so configured sizes (head, tail, and the
`CompressionBacklogSize` no-compress threshold on `scrollback.Config`) are
approximate.
`Snapshot` decompresses head then tail, and when the middle was discarded it
inserts a demarcation block (empty line, marker, `[...] truncated <humanized>`,
marker, empty line) and a RIS reset preamble right before the tail so the tail
renders on a clean state. A session whose head retains everything is byte-for-byte
unchanged, keeping the attach torture test valid.

The daemon feeds `s.term` only up to the latest ground/rune boundary
(`feedTerm`), buffering the trailing partial sequence for the next read while
still storing and fanning out every raw byte, so `serveAttach`'s `DumpScreen`
snapshot is always taken at a clean boundary that tiles with the streamed
remainder.
