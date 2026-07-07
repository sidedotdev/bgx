---
intent_links:
  - intent: "#release"
    code:
      - scripts/release.sh
  - intent: "#github-actions"
    code:
      - .github/workflows/build.yml
  - intent: "#verification-process"
    code:
      - scripts/release.sh
---
# Build Process

## Release

A github build/release can be triggered via manually invoking a github actions
workflow or on tag push. A build is promoted from pre-release to release after
all tests have passed, and builds have been uploaded, unless opted out.

A release script invokes `gh` to create a release, given a ref or tag:

- Auto-incrementing minor tag using HEAD of main if not specified
- Waits for the workflow to complete
- On failure, syncs detailed logs from github actions to an in-repo gitignored
directory, enabling grep debugging.
- An option exists to list previous runs for a given ref or tag, and

## Github Actions

- Native runners are used rather than cross-compilation.
- Builds supported:
  - linux amd64
  - linux arm64
  - darwin arm64
- Runs tests
- Creates static builds of the cli tool across platforms
- Uploads the builds to the release

## Verification Process

Changes to the workflow are tested by creating an alpha pre-release that isn't
promoted. This ensures the workflow finishes successfully by checking for
uploaded artifacts in the release after the workflow ends, and downloading a
build for the current platform to run a smoke test with.