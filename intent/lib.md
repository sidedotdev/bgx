---
intent_links:
  - intent: "#lib"
    code:
      - main.go:Command
      - main.go:Run
      - attach.go
      - attachview.go
      - client.go
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

## Command-level

`bgx` exposes the urfave/cli command, subcommands and their handlers publicly,
with the intent being for other libs to be able to trivially embed these within
their binaries when needed.

## Lower-level

Useful utilities to interact as a client of bgx streams is provided. This
includes:
