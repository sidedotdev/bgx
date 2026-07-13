# Release build (generated)

Log of the v0.2.0 release-build investigation and the durable fixes applied to
`.github/workflows/build.yml`. The workflow cross-builds the static
`libghostty-vt` archive per target with zig, then links it via cgo/pkg-config.

## Symptom

Release runs intermittently failed at the cgo link with:

    ld: cannot find .../build/_deps/ghostty-src/zig-out/lib/libghostty-vt.a

i.e. pkg-config resolved the archive to zig's default `zig-out` install layout
rather than the cross-target install dir where the archive actually landed.

## Root causes (three independent; all required fixing)

1. Retired runner. `macos-13` is no longer schedulable, so the `darwin/amd64`
   job queued forever and the tagged release never completed. Switched to
   `macos-15-intel`.

2. Nondeterministic generated `.pc`. zig bakes absolute install paths into the
   generated pkg-config files and, depending on build-cache state, emits paths
   under its default `zig-out` layout instead of the cross `--prefix`. Trusting
   those paths (even with a `prefix=` rewrite) is unreliable. Fix: regenerate a
   self-contained static `.pc` whose location vars and archive token are pinned
   to the real absolute paths, confine pkg-config to that dir via
   `PKG_CONFIG_LIBDIR`, and assert at build time that resolution points at the
   real archive (turning silent nondeterminism into an early, self-diagnosing
   failure).

3. Poisoned go build cache. go's build-cache key does not include
   `PKG_CONFIG_PATH`, so a cgo package compiled against a stale (`zig-out`) `.pc`
   is reused even after the `.pc` is corrected, so the link keeps using the
   stale path. This is the reason a fresh `pkg-config` (the build-step
   assertion) resolved correctly while `go test` still linked `zig-out`. Fix:
   `cache: false` on `actions/setup-go`. Relatedly, `use-cache: false` on
   `mlugg/setup-zig` avoids carrying a stale zig cache into `.pc` generation.

## Testing note

`workflow_dispatch` runs on the feature branch read that branch's caches and can
mask the poison (a clean go cache resolves correctly), whereas tag-push runs use
a different cache scope and expose it. Validate release changes via the tag-push
path, not only via dispatch.