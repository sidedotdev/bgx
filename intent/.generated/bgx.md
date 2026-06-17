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