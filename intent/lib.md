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

# bgx library

The bgx library allows any golang app to:

1. Run and interact with local bgx sessions
2. Run and interact with remote bgx sessions
3. Bridge local bgx sessions to remote clients

These capabilities together enable a library-based bgx client to interact with
library-managed remote bgx sessions (i.e. via a remote go binary that isn't
bgx), and all other combinations of library vs cli and local vs remote.