# v0.2.0 release-build debugging log

Ordered log of attempts, hypotheses, and outcomes. Branch:
`side/unrelease-and-fix-issues`. Workflow: `.github/workflows/build.yml`.

Symptom: cgo link fails with
`ld: cannot find .../build/_deps/ghostty-src/zig-out/lib/libghostty-vt.a` —
pkg-config resolves the archive to zig's default `zig-out` layout instead of the
cross-target install dir. Failure was intermittent.

## Attempts (ordered)

1. sha 8e9c7d5 "Pin cross-target pkg-config prefix" — rewrite `prefix=` in the
   generated `.pc`. tag run 29188874509 FAILED (fresh) with zig-out path.

2. sha d2ba4d4 debug dump. dispatch run 29189061659 PASSED. Misleading: it hit a
   libghostty cache from an earlier build, masking the issue.
   Hypothesis "stale zig global cache poisons .pc" formed.

3. sha caeaf8a `use-cache: false` on setup-zig + bump libghostty cache key.
   tag run 29189801844 FAILED (fresh). Disproves that zig-cache alone was it.

4. sha b86ffec debug. dispatch 29190822621 (cache hit) showed `.pc` was correct.
   dispatch 29191055919 (fresh, after deleting v2 caches) PASSED — fresh looked
   fine here, but nondeterministically.

5. sha 0fa3042 remove debug. tag run 29191197711 FAILED (fresh, zig-out).
   Confirms fresh builds are themselves nondeterministic, not just cache hits.

6. sha 3a46f5f debug trace `pkg-config --debug` + `find`. dispatch 29191783966
   PASSED; dump showed a second `.pc` under the workspace `.zig-cache/` (created
   by mlugg/setup-zig), both copies with correct prefix on that run.

7. sha 518ef23 "Confine pkg-config via PKG_CONFIG_LIBDIR" — copy corrected `.pc`
   into a dedicated dir and set PKG_CONFIG_LIBDIR. dispatch 29240530634 PASSED
   3/4; 29240849018 (cache hit) PASSED linux. darwin/amd64 stuck queued.

8. Discovered `macos-13` runner is retired -> darwin/amd64 never scheduled.
   sha d9fcfda switch to `macos-15-intel`. dispatch 29241344271 PASSED all 4
   (fresh). tag run 29243215180 FAILED 3/4 (fresh cache-miss, zig-out).
   Note: tag runs get a different cache scope than branch dispatch runs, so tag
   runs exposed failures that dispatch runs masked.

9. sha e916279 "Rewrite static .pc to absolute archive path + assert" — regen a
   self-contained `.pc` (absolute archive), assert pkg-config resolves it at
   build time. dispatch 29246238211 PASSED all 4. tag run 29246927481 FAILED
   3/4 — but the build-step assertion PASSED while `go test` still linked
   zig-out. Key clue: a fresh pkg-config call resolved correctly, yet the link
   used the stale path.

10. Root cause identified: go's build cache. Its cache key excludes
    PKG_CONFIG_PATH, so a cgo package compiled against a stale (zig-out) `.pc`
    is reused even after the `.pc` is corrected. setup-go restores that poisoned
    cache across runs; dispatch runs passed only when their go cache was clean.
    sha 82f4b1b `cache: false` on actions/setup-go.

11. sha e7b3f34 current tag/release run 29247640710 validating the go-cache fix
    on the tag path.

## Durable fixes (in build.yml)

- darwin/amd64 runner `macos-13` -> `macos-15-intel`.
- `use-cache: false` on mlugg/setup-zig (no stale zig cache into .pc gen).
- Regenerate a self-contained static `.pc` with absolute archive path, confine
  pkg-config via PKG_CONFIG_LIBDIR, assert resolution at build time.
- `cache: false` on actions/setup-go (the decisive fix for the poison).

## Testing note

Validate release changes on the tag-push path, not only via workflow_dispatch:
dispatch runs read the branch cache scope and can mask cache-poisoning bugs.