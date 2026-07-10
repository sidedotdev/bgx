---
intent_links:
  - intent: "#github-actions"
    code:
      - .github/workflows/build.yml
      - scripts/release.sh:EXPECTED_ASSETS
  - intent: "#verification-process"
    code:
      - scripts/release.sh:smoke_test
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
- Native dependencies in release artifacts must target the supported
  architecture baseline rather than optional CPU features of the build runner.
- All dependencies/actions/etc are pre-cached to the extent possible
- Platforms supported:
  - linux amd64
  - linux arm64
  - darwin amd64
  - darwin arm64
- On each platform:
  - Runs unit tests
  - Creates static builds of the cli tool
  - Runs full suite of black-box tests on static build
  - Uploads the build to the release

## Verification Process

Changes to the workflow are tested by creating an alpha pre-release that isn't
promoted. This ensures the workflow finishes successfully by checking for
uploaded artifacts in the release after the workflow ends, and downloading a
build for the current platform to run a smoke test with.