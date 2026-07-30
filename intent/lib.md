---
intent_links:
  - intent: "#lib"
    code:
      - main.go:Command
      - main.go:Run
      - main.go:Main
      - main.go:DaemonCommand
      - attach.go
      - attachview.go
      - client.go
      - commands.go
      - commands_test.go:TestExportedSubcommands
      - ctrlkey.go
      - ctrlkey_test.go
      - dirs.go
      - errors.go
      - lib_test.go
      - cmd/bgx/main.go
      - e2e/main_test.go:run
      - .github/workflows/build.yml
---

# Lib

`bgx` as a lib at minimum exposes the urfave/cli command, subcommands and their
handlers publicly, with the intent being for other libs to be able to trivially embed these within their binaries when needed.