---
intent_links:
  - intent: "#github-actions"
    code:
      - .github/workflows/build.yml
---

# Build Process (generated)

Concise record of consequential decisions inferred while implementing
`intent/builds.md`. The human-authored intent remains the source of truth; this
file captures design choices not spelled out there.

## Github Actions

Native runners determine the release artifact's operating system and
architecture, but release dependencies must not inherit optional CPU features
exposed by a particular runner. The Linux ARM64 workflow therefore compiles
libghostty for Zig's baseline `aarch64-linux-gnu` target and links bgx against
that output. Other platforms continue to use libghostty's native build.