---
intent_links:
  - intent: "#detach-completion"
    code:
      - client_attach.go:runTerminalAttach
      - client_attach_test.go:TestClientAttachDetachHandlesPeerEOFBeforeWriteReturns
      - attach_api_test.go:TestAttachAPIUsesLocalSessionAndOptions
  - intent: "#attach-lifecycle-verification"
    code:
      - client_attach_fuzz_test.go:FuzzClientAttachShutdown
  - intent: "#escape-continuation"
    code:
      - client_attach_input.go:runAttachInput
      - ctrlkey.go:ctrlBackslashPrefix
      - client_attach_escape_test.go
  - intent: "#release-retry"
    code:
      - scripts/release.sh:main
      - release_script_test.go:TestReleaseScriptRetriesOnlyFailedJobs
---
# Detach completion

A successful detach permits peer EOF even before the detach write returns.
Unrelated concurrent errors must remain observable.

# Release retry

Each release-script invocation retries a confirmed failed build once, preserving
successful jobs. A failed retry stops promotion and synchronizes logs for diagnosis.

# Attach lifecycle verification

Fuzzing varies input boundaries and concurrent shutdown errors across reproducible
detach, peer-disconnect, and cancellation orderings. Error retention, input
forwarding, terminal restoration, and stream closure are checked together.

# Escape continuation

A lone Escape waits up to 25 ms for a continuation before being forwarded.
Longer partial CSI sequences retain their existing buffering behavior.
EOF flushes pending input immediately. Escape expiry must not cancel or close
the terminal reader; shutdown must join that reader before restoring the terminal.