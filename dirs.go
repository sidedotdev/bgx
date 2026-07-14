package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/adrg/xdg"
)

// dirCandidate is a base directory bgx may use for its sockets and retained
// records, paired with a human-readable name for diagnostics.
type dirCandidate struct {
	name string
	path string
}

// dirResolution is the memoized outcome of walking the base-directory fallback
// chain: the chosen base plus a notice describing any fallback, or an error
// when every candidate was unusable.
type dirResolution struct {
	base   string
	notice string
	err    error
}

var (
	dirOnce   sync.Once
	dirResult dirResolution
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

// dirCandidates builds the ordered, de-duplicated list of base directories to
// try. It prefers an explicitly set $XDG_RUNTIME_DIR, then the default XDG
// runtime dir, then $HOME/.bgx, then /tmp/bgx, and finally ./.bgx as a last
// resort.
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
	// The default XDG runtime dir is preferred whenever $XDG_RUNTIME_DIR is
	// unset or unusable, so it follows the explicit value as its own candidate.
	if xdg.RuntimeDir != "" {
		add("default XDG runtime dir", filepath.Join(xdg.RuntimeDir, "bgx"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add("$HOME/.bgx", filepath.Join(home, ".bgx"))
	}
	add("/tmp/bgx", filepath.Join(os.TempDir(), "bgx"))
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		add("./.bgx", filepath.Join(cwd, ".bgx"))
	}
	return out
}

// computeDirs tries each candidate in order, idempotently creating it and
// probing it for write access. The first usable candidate wins; if it is not
// the preferred one, a notice records what was skipped and why.
func computeDirs() dirResolution {
	candidates := dirCandidates()
	var attempts []string
	for i, c := range candidates {
		if err := usableDir(c.path); err != nil {
			attempts = append(attempts, fmt.Sprintf("%s (%s): %v", c.name, c.path, err))
			continue
		}
		notice := ""
		if i > 0 {
			notice = fmt.Sprintf("bgx: %s unusable, falling back to %s (%s); skipped: %s",
				candidates[0].name, c.name, c.path, strings.Join(attempts, "; "))
		}
		return dirResolution{base: c.path, notice: notice}
	}
	return dirResolution{err: fmt.Errorf("all base directory fallbacks failed: %s", strings.Join(attempts, "; "))}
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
	probe.Close()
	os.Remove(probe.Name())
	return nil
}

// ensureDirs reports whether a usable base directory was found, returning a
// clear error only when every fallback failed.
func ensureDirs() error {
	return resolveDirs().err
}

// fallbackNotice returns a human-readable description of any fallback that
// occurred, or the empty string when the preferred directory was used.
func fallbackNotice() string {
	return resolveDirs().notice
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
// grouped by id namespace beneath it. It returns the empty string when
// resolution failed so callers never fabricate a relative path.
func retentionDir() string {
	base := resolveDirs().base
	if base == "" {
		return ""
	}
	return filepath.Join(base, "ended")
}
