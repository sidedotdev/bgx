---
intent_links:
  - intent: "#bgx-library"
    code:
      - main.go:version
      - internal/cli/cli.go:runner.rootCommand
      - internal/cli/cli_test.go
      - e2e/run_test.go:removeRunDir
      - attach.go:AttachOptions
      - attach.go:Attach
      - attach_api_test.go
      - command_api.go
      - command_api_test.go
      - internal/cli/cli.go
      - internal/cli/cli_test.go
      - lib_test.go
      - cmd/bgx/main.go
  - intent: "#api"
    code:
      - dirs.go:EnsureDirs
      - internal/cli/cli.go
      - internal/cli/cli_test.go
      - internal/cli/cli_test.go:TestRootCommandsHaveLibraryOperations
      - internal/cli/cli_test.go:TestRootCommandFlagsHaveMirroredOptionFields
  - intent: "#constraints"
    code:
      - dirs.go:EnsureDirs
      - internal/cli/cli.go
      - internal/cli/cli_test.go
      - internal/cli/cli_test.go:TestRootCommandsHaveLibraryOperations
      - internal/cli/cli_test.go:TestRootCommandFlagsHaveMirroredOptionFields
      - lib_test.go:TestLibraryDependencyGraphExcludesCLIFramework
      - lib_test.go:TestPublicSurfaceHasNoCLIFrameworkTypes
      - lib_test.go:TestCLIArgvAdapterSignatureDetection
  - intent: "#client"
    code:
      - client_api.go:Dialer
      - client_api.go:SessionInfo
      - client_api.go:ExitResult
      - client_api.go:ResponseError
      - client_api.go:ProtocolError
      - client_api.go:Client
      - client_api.go:NewClient
      - client_api.go:Client.Info
      - client_api.go:Client.Wait
      - client_api.go:Client.Kill
      - client_api.go:Client.Send
      - client_api.go:Client.History
      - client_attach.go:AttachOption
      - client_attach.go:WithDetachInstructions
      - client_attach.go:Client.Attach
      - attach.go:AttachOptions
      - attach.go:Attach
      - attach_api_test.go
      - client_attach.go:withoutExpectedAttachShutdownErrors
      - client_attach_test.go
      - attachview_test.go:TestAttachViewCloseRetainsAsynchronousPaintError
      - terminal.go:ErrTerminalSizeUnavailable
      - terminal.go:Terminal
      - terminal.go:ProcessTerminal
      - terminal.go:ProcessTerminal.Size
      - terminal_size_unix.go:normalizeTerminalSizeError
      - terminal_size_windows.go:normalizeTerminalSizeError
      - terminal.go:ProcessTerminal.Size
      - terminal_size_unix.go:normalizeTerminalSizeError
      - terminal_size_unix_test.go:TestNormalizeTerminalSizeError
      - terminal_size_windows.go:normalizeTerminalSizeError
      - terminal_size_windows_test.go:TestNormalizeTerminalSizeError
      - terminal_read_unix.go:ProcessTerminal.ReadContext
      - terminal_read_windows.go:ProcessTerminal.ReadContext
      - terminal_resize_unix.go:ProcessTerminal.ResizeEvents
      - terminal_resize_windows.go:ProcessTerminal.ResizeEvents
      - terminal_read_windows_test.go:TestProcessTerminalReadContextCancellationWithBlockedInput
      - terminal_read_windows_test.go:TestClientAttachSessionEndCancelsBlockedProcessTerminalInput
      - terminal.go:NewProcessTerminal
      - client.go:sessionClient
      - client_api_test.go
  - intent: "#sessions"
    code:
      - start.go:RunSpec
      - start.go:RunSpec.startOptions
      - start.go:Run
      - start.go:StartOptions
      - start.go:Start
      - start.go:ErrSessionRunning
      - start.go:ErrSessionExists
      - start.go:ConcurrencyLimitError
      - start.go:StartupError
      - start_test.go
      - start_test.go:TestStartTimeoutDoesNotCancelDetachedSession
      - start_test.go:TestRunSpecZeroValueInheritsWorkingDirectory
      - daemon/daemon.go:Config
      - daemon/daemon.go:environmentWithOverrides
      - daemon/daemon.go:Session.start
      - daemon/daemon_test.go:TestSessionStartAppliesDirectoryAndEnvironmentOverrides
      - sessions.go:ListOptions
      - sessions.go:ListSessions
      - sessions.go:ListRunning
      - sessions.go:ListEnded
      - sessions.go:EndedRecord
      - sessions_test.go
  - intent: "#daemonization"
    code:
      - daemonize.go:InterceptDaemon
      - client.go:spawnDaemon
      - cmd/bgx/main.go
  - intent: "#bridging"
    code:
      - transport.go:Dial
      - transport.go:Bridge
      - transport_test.go
  - intent: "#compatibility"
    code:
      - client_attach.go:Client.Attach
      - client_attach_test.go:TestClientAttachIgnoresUnknownDaemonFrames
      - daemon/attach.go:Session.serveAttach
      - daemon/daemon_test.go:TestAttachIgnoresUnknownClientFrames
---

# bgx library

bgx is importable as a Go library. The module root package
(`github.com/sidedotdev/bgx`) is the public surface; the standalone CLI is a
thin shim in `cmd/bgx` with unchanged behavior and JSON contracts. No
CLI-framework types appear in the public API.

## Sessions
- Typed session start with `run`-equivalent semantics: scrollback/retention/metadata options, per-namespace concurrency cap, readiness wait, startup-error surfacing.
- A zero-value run specification inherits the caller's working directory and full environment. Environment entries in the specification overlay inherited values.
- Startup timeout bounds only readiness observation; timing out does not cancel or terminate the detached session.
- List running and ended sessions (metadata filtering); ended-record lookup.

## Client
- A typed client exposes `Info`, `Wait`, `Kill`, `Send`, `History`, `Attach`.
- It is parameterized by a `Dialer func(ctx) (io.ReadWriteCloser, error)`; each operation consumes one fresh stream speaking the existing socket protocol. The same client serves local and remote sessions; only the Dialer differs.
- `Attach` accepts a terminal abstraction (read/write, size, resize notification, raw-mode enter/restore); a process-TTY implementation is provided as the default. The CLI's interactive attach is built on it.

One socket connection carries exactly one operation (attach consumes its
connection permanently), so the client takes a per-operation Dialer rather than
one long-lived stream; a single permanent transport would have required a new
multiplexing agent protocol, which was rejected to keep the cross-version
compatibility surface at the existing JSON + frame protocols. `Dial` doubles as
the local Dialer, so local and remote use share one client.

## Bridging
- `Dial(ctx, id)` returns one connection to a local session's socket; it is the local Dialer.
- `Bridge(ctx, id, rw)` pipes a caller-supplied stream to/from one socket connection until EOF, so a host process on a remote machine can act as a bgx server agent via the library alone.

## Daemonization
- Host binaries call an intercept hook (e.g. `bgx.InterceptDaemon()`) first in `main()`: a no-op normally; when the process was re-exec'd by session start, it runs the session daemon and exits. `cmd/bgx` uses the same hook.

Session start re-execs the host executable with a marker recognized by
`InterceptDaemon` (replacing the CLI-only hidden `__daemon` subcommand), so any
binary embedding the library can serve as its own daemon without mounting bgx
commands. Exporting the urfave/cli command tree was considered and rejected;
embedding is typed-library-only.

## Compatibility
- Frame receivers ignore frames with unknown tags.
- The JSON attach handshake is the sole extension point for future version/capability negotiation.

Both frame endpoints already dropped unknown tags; that behavior is now a
declared contract, with the JSON attach handshake reserved as the only
negotiation point, keeping additive protocol evolution (new tags, new JSON
fields) skew-safe while multiplexing/compression/auth remain the transport's
concern.
