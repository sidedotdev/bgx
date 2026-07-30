---
intent_links:
  - intent: "#lib"
    code:
      - attach.go
      - attachview.go
      - client.go
      - commands.go
      - commands_test.go:TestExportedSubcommands
      - ctrlkey.go
      - ctrlkey_test.go
      - dirs.go
      - errors.go
      - main.go:Main
      - main.go:newApp
      - main.go:DaemonCommand
      - cmd/bgx/main.go
      - e2e/main_test.go:run
      - .github/workflows/build.yml
---

# Lib

`bgx` as a lib at minimum exposes the urfave/cli command and subcommands publi.