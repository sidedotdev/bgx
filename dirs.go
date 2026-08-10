package bgx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/adrg/xdg"
)

// dirCandidate pairs a directory with a human-readable name for diagnostics.
type dirCandidate struct {
	name string
	path string
}

// dirResolution is the memoized outcome of walking a directory fallback chain.
type dirResolution struct {
	base   string
	notice string
	err    error
}

var (
	dirOnce     sync.Once
	dirResult   dirResolution
	stateOnce   sync.Once
	stateResult dirResolution
)

// resolveDirs walks the fallback chain once per process, logging any fallback
// notice to stderr exactly once so downstream JSON output can echo the same
// metadata without repeating the log.
func resolveDirs() dirResolution {
	dirOnce.Do(func() {
		dirResult = computeDirs()
		if dirResult.notice != "" {
			fmt.Fprintln(os.Stderr, dirResult.notice)
		}
	})
	return dirResult
}

// dirCandidates builds the ordered, de-duplicated list of socket base
// directories. It prefers an explicitly set $XDG_RUNTIME_DIR, then the default
// XDG runtime dir, then $HOME/.bgx, /tmp/bgx, and ./.bgx.
func dirCandidates() []dirCandidate {
	var out []dirCandidate
	seen := map[string]bool{}
	add := func(name, path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, dirCandidate{name: name, path: path})
	}

	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		add("$XDG_RUNTIME_DIR", filepath.Join(v, "bgx"))
	}
	if xdg.RuntimeDir != "" {
		add("default XDG runtime dir", filepath.Join(xdg.RuntimeDir, "bgx"))
	}
	addCommonDirCandidates(&out, seen)
	return out
}

// computeDirs resolves the socket base-directory fallback chain.
func computeDirs() dirResolution {
	return computeDirResolution(dirCandidates(), "runtime")
}

// usableDir idempotently creates dir and verifies it is writable by creating
// and removing a probe file, so a directory that exists but denies writes is
// treated as unusable.
func usableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	probe, err := os.CreateTemp(dir, ".bgx-probe-*")
	if err != nil {
		return err
	}
	return errors.Join(probe.Close(), os.Remove(probe.Name()))
}

// ensureDirs prepares independently resolved locations for sockets and retained
// session data.
func ensureDirs() error {
	return errors.Join(resolveDirs().err, resolveStateDir().err)
}

// fallbackNotice returns a human-readable description of runtime or state
// directory fallbacks.
func fallbackNotice() string {
	var notices []string
	if notice := resolveDirs().notice; notice != "" {
		notices = append(notices, notice)
	}
	if notice := resolveStateDir().notice; notice != "" {
		notices = append(notices, notice)
	}
	return strings.Join(notices, "; ")
}

// socketDir is where per-session unix domain sockets live, beneath the resolved
// base directory. It returns the empty string when resolution failed so callers
// never fabricate a relative path.
func socketDir() string {
	base := resolveDirs().base
	if base == "" {
		return ""
	}
	return filepath.Join(base, "run")
}

// retentionDir holds persisted records and histories for ended sessions,
// grouped by id namespace beneath the resolved state directory.
func retentionDir() string {
	base := resolveStateDir().base
	if base == "" {
		return ""
	}
	return filepath.Join(base, "ended")
}

// EnsureDirs resolves and prepares the directories used by session operations.
func EnsureDirs() error {
	return ensureDirs()
}

// resolveStateDir walks the state-directory fallback chain once per process.
func resolveStateDir() dirResolution {
	stateOnce.Do(func() {
		stateResult = computeDirResolution(stateDirCandidates(), "state")
		if stateResult.notice != "" {
			fmt.Fprintln(os.Stderr, stateResult.notice)
		}
	})
	return stateResult
}

// stateDirCandidates builds the ordered, de-duplicated list of retained-data
// base directories.
func stateDirCandidates() []dirCandidate {
	var out []dirCandidate
	seen := map[string]bool{}
	add := func(name, path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, dirCandidate{name: name, path: path})
	}

	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		add("$XDG_STATE_HOME", filepath.Join(v, "bgx"))
	}
	if xdg.StateHome != "" {
		add("default XDG state dir", filepath.Join(xdg.StateHome, "bgx"))
	}
	addCommonDirCandidates(&out, seen)
	return out
}

func addCommonDirCandidates(out *[]dirCandidate, seen map[string]bool) {
	add := func(name, path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		*out = append(*out, dirCandidate{name: name, path: path})
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add("$HOME/.bgx", filepath.Join(home, ".bgx"))
	}
	add("/tmp/bgx", filepath.Join(os.TempDir(), "bgx"))
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		add("./.bgx", filepath.Join(cwd, ".bgx"))
	}
}

// computeDirResolution selects the first writable candidate and records enough
// context to diagnose any fallback.
func computeDirResolution(candidates []dirCandidate, kind string) dirResolution {
	var attempts []string
	for i, candidate := range candidates {
		if err := usableDir(candidate.path); err != nil {
			attempts = append(attempts, fmt.Sprintf("%s (%s): %v", candidate.name, candidate.path, err))
			continue
		}
		notice := ""
		if i > 0 {
			notice = fmt.Sprintf("bgx: %s unusable, falling back to %s (%s); skipped: %s",
				candidates[0].name, candidate.name, candidate.path, strings.Join(attempts, "; "))
		}
		return dirResolution{base: candidate.path, notice: notice}
	}
	return dirResolution{
		err: fmt.Errorf("all base directory fallbacks failed (%s): %s", kind, strings.Join(attempts, "; ")),
	}
}
