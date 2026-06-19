---
intent_links:
  - intent: "#daemonization"
    code:
      - main.go:daemonCommand
      - main.go:daemonAction
      - client.go:spawnDaemon
  - intent: "#socket-protocol"
    code:
      - daemon/protocol.go
      - daemon/frame.go
      - client.go:dialRequest
      - daemon/attach.go
  - intent: "#terminal-state"
    code:
      - vt/vt.go:Terminal
      - daemon/daemon.go:Session
  - intent: "#attach-handoff"
    code:
      - daemon/attach.go:serveAttach
      - daemon/daemon.go:pumpOutput
      - daemon/daemon_test.go:TestAttachSnapshotStreamCoversEntireOutput
  - intent: "#send-and-wait-semantics"
    code:
      - client.go:sendAction
      - client.go:waitAction
      - client.go:emitExit
  - intent: "#configuration-and-layout"
    code:
      - main.go:socketDir
      - main.go:retentionDir
      - client.go:socketPath
      - daemon/retention.go:Namespace
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

## Socket protocol

One unix domain socket per session lives under the XDG runtime dir (tmp
fallback), with the session id encoded into a single safe filename component.
Clients speak a JSON-line request/response protocol (`info`, `wait`, `kill`,
`send`, `history`). `attach` upgrades the same connection, after the JSON
handshake, to tagged length-prefixed binary frames (Input/Output/Resize/Detach)
for raw bidirectional bridging. `history` is returned as base64 in the JSON
response and written raw to stdout by the client.

## Terminal state

The daemon maintains a libghostty-vt terminal fed every PTY output byte
alongside the scrollback store, fixed at an 80x24 default size. Attach uses
`DumpScreen` for the initial snapshot; the attach leader controls PTY and vt
size via Resize frames. The daemon answers Device Attributes queries itself when
no client is attached so interactive programs don't hang.

## Attach handoff

A client joining a live session receives a point-in-time `DumpScreen` snapshot
followed by the raw output stream. To avoid losing or duplicating output
produced between rendering the snapshot and subscribing to the stream,
`serveAttach` captures the snapshot and joins the output fanout under the same
`outMu` that `pumpOutput` holds while writing each chunk to the terminal and
fanning it out. Each PTY chunk therefore lands entirely before the snapshot
(reflected in it, not streamed) or entirely after the subscription (streamed,
not in the snapshot), so a client's snapshot and stream tile the full session
output with no gap or overlap. `TestAttachSnapshotStreamCoversEntireOutput` is
the torture test guarding this invariant.

## Send and wait semantics

`send` writes exactly the argv joined by single spaces as raw PTY bytes — no
trailing newline and no completion markers; callers send line endings
themselves. `wait` returns the exit code as JSON and additionally exits the bgx
process with that same code.

## Configuration and layout

Scrollback head/tail sizes, storage kind/path, and retention count are
configured via `run` flags (with env fallbacks) and forwarded to the daemon.
Ended-session records and histories are persisted under a tmp retention dir,
grouped by id namespace (the substring before the first "/"; slashless ids share
one global namespace), keeping only the newest N (default 10) per namespace.

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