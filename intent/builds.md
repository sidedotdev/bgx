# Build Process

## Release

A github build/release can be triggered via manually invoking a github actions
workflow. A build is promoted from pre-release to release after all tests have
passed, and builds have been uploaded.

A release script invokes `gh` to create a release, given a ref or tag:

- Auto-incrementing minor tag using HEAD of main if not specified
- Waits for the workflow to complete.
- - On
failure, syncs detailed logs from github actions to an in-repo gitignored
directory, enabling grep debugging. An option exists to list previous runs for a
given ref or tag, and

## Github Actions

- Native runners are used rather than cross-compilation.
- Builds supported match what iroh-ffi supports:
  - linux amd64
  - linux arm64
  - darwin arm64
  - windows amd64