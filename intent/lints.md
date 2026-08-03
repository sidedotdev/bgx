---
intent_links:
  - intent: "#golangci-lint"
    code:
      - .github/workflows/build.yml
      - .golangci.yml
      - side.yml
      - client.go
      - client_api.go:Client.request
      - client_api_test.go
      - daemon/attach.go:Session.deliverOutput
      - daemon/daemon.go
      - daemon/daemon_test.go
      - daemon/retention.go
      - daemonize.go:InterceptDaemon
      - dirs.go
      - error_output_test.go
      - errors.go
      - e2e/attach_errors_test.go
      - e2e/attach_test.go
      - e2e/bridge_test.go
      - e2e/filesystem_test.go
      - e2e/main_test.go
      - e2e/run_test.go
      - main.go
      - scrollback/backend.go
      - scrollback/backend_test.go
      - scrollback/store.go
      - scrollback/store_test.go
      - sessions_test.go:TestListSessionsLiveSessionShadowsEndedRecord
      - start.go
      - start_test.go
      - transport.go
      - transport_test.go
      - vt/vt_test.go
---

# Lints

## golangci-lint

- Is run on each build
- Present in side.yml's test commands
- Configured to disallow ignoring errors