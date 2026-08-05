---
intent_links:
  - intent: "#bgx-library"
    code:
      - main.go:version
      - internal/cli/cli.go:runner.rootCommand
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
      - start.go:RunSpec
      - start.go:RunSpec.startOptions
      - start.go:Run
      - start_test.go:TestRunMirrorsCLIOptions
      - start_test.go:TestStartTimeoutDoesNotCancelDetachedSession
      - start_test.go:TestRunSpecZeroValueInheritsWorkingDirectory
      - attach.go:AttachOptions
      - attach.go:Attach
      - attach_api_test.go
      - command_api.go
      - command_api_test.go
      - dirs.go:EnsureDirs
      - internal/cli/cli.go
      - internal/cli/cli_test.go
      - internal/cli/cli.go:runner.runAction
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
      - lib_test.go:TestLibraryDependencyGraphExcludesCLIFramework
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

The bgx API provides methods that mirror cli subcommands. Arguments/options are
very roughly mirrored as well, but only when appropriate and in a way that
matches idiomatic golang. We don't require 1:1 match, preferring the CLI and API
to be ergonomic and match local conventions.

For example `bgx.Run` is the library equivalent of the `bgx run` cli subcommand,
and supporting overwriting the ID makes sense, but supporting `async` does not:
`bgx.Run` is instead only async and can be made sync by programmatically
invoking `bgx.Wait`. In addition, `bgx.Run` does not take in a `ctx` to avoid
confusion around what is being canceled (as the underlying session is not tied
to the ctx), so we use a separate `StartupTimeout option` instead. However
`bgx.Wait` does take a `ctx`.

However, similar to the cli, reasonable defaults are set for all options when
they are not explicitly configured. These two usage examples demonstrate the
desired surface and style. Example 1, most basic usage:

```go
// Run is always async
// Default options use env and cwd of current process and cli defaults for rest if not set
sessionInfo, err := bgx.Run("build-123", []string{"make", "all"}, bgx.RunSpec{}) 
```

Here is a more complex configuration:

```go
sessionInfo, err := bgx.Run(id, arvg, bgx.RunSpec{
    Dir:           "/mnt/workspace",
    TailSize:      1 << 20,
    Env:           map[string]string{"FOO": "1"},
    StartTimeout:  time.Second,
})

res, err := bgx.Wait(ctx, id)
```

## Constraints

- The library is at least as powerful as the cli: the cli can do nothing that
  the library does not expose. This is not necessarily true vice versa.
- The cli itself is NOT exposed via the library, e.g. we do not want the library
  to expose a way to run the top-level cli command action.
- The lib does not depend on urfave/cli, so bgx consumers don't transitively
  depend on it
- Downstream lib dependents DO NOT need a zig toolchain, CGO suffices