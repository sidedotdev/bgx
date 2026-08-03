---
intent_links:
  - intent: "#bgx-library"
    code:
      - main.go:rootCommand
      - commands.go
      - commands_test.go
      - attach.go
      - attachview.go
      - client.go
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
      - client_attach_test.go
      - client_api_test.go
      - transport.go:Dial
      - transport.go:Bridge
      - transport_test.go
      - start.go:StartOptions
      - start.go:Start
      - start.go:ConcurrencyLimitError
      - start.go:StartupError
      - start_test.go
      - sessions.go:ListOptions
      - sessions.go:ListSessions
      - sessions.go:ListRunning
      - sessions.go:ListEnded
      - sessions.go:EndedRecord
      - sessions_test.go
      - daemonize.go:InterceptDaemon
      - terminal.go:Terminal
      - terminal.go:ProcessTerminal
      - terminal.go:NewProcessTerminal
      - ctrlkey.go
      - ctrlkey_test.go
      - dirs.go
      - errors.go
      - lib_test.go
      - cmd/bgx/main.go
      - e2e/attach_errors_test.go
      - e2e/main_test.go:run
      - .github/workflows/build.yml
  - intent: "#api"
    code:
      - start.go:RunOptions
      - start.go:Run
      - attach.go:AttachOptions
      - attach.go:Attach
      - attach_api_test.go
      - command_api.go
      - command_api_test.go
      - dirs.go:EnsureDirs
      - internal/cli/cli.go
      - internal/cli/cli_test.go
      - client.go:runAction
      - start_test.go:TestRunMirrorsCLIOptions
      - internal/cli/cli_test.go:TestRootCommandsHaveLibraryOperations
      - internal/cli/cli_test.go:TestRootCommandFlagsHaveMirroredOptionFields
  - intent: "#constraints"
    code:
      - attach.go:Attach
      - attach_api_test.go
      - command_api.go
      - command_api_test.go
      - dirs.go:EnsureDirs
      - internal/cli/cli.go
      - internal/cli/cli_test.go
      - cmd/bgx/main.go:main
      - internal/cli/cli_test.go:TestRootCommandsHaveLibraryOperations
      - internal/cli/cli_test.go:TestRootCommandFlagsHaveMirroredOptionFields
      - lib_test.go:TestPublicSurfaceHasNoCLIFrameworkTypes
      - lib_test.go:TestCLIArgvAdapterSignatureDetection
---

# bgx library

The bgx library allows any golang app to:

1. Run and interact with local bgx sessions
2. Run and interact with remote bgx sessions
3. Bridge local bgx sessions to remote clients

These capabilities together enable a library-based bgx client to interact with
library-managed remote bgx sessions (i.e. via a remote go binary that isn't
bgx), and all other combinations of library vs cli and local vs remote.

## API

The bgx API provides an interface that mirrors the cli. For example `bgx.Run` is
the library equivalent of the `bgx run` cli subcommand. Option names should be
mirrored as well.

## Constraints

- The library is at least as powerful as the cli: the cli can do nothing that
  the library does not expose. This is not necessarily true vice versa.
- The cli itself is NOT exposed via the library, e.g. we do not want the library
  to expose a way to run the top-level cli command action.