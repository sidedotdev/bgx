package e2e

// These tests validate the '### Filesystem Requirements' of intent/bgx.md: the
// XDG-preferred base-directory fallback chain, its stderr+JSON fallback
// metadata, and the clear error when every candidate fails. Access issues are
// simulated with a platform sandbox (seatbelt on macOS, bubblewrap on Linux)
// and missing directories with docker, per the '## Testing / Verification'
// note. Each test skips when its required tool/platform is unavailable so the
// suite stays green in constrained environments.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// captureCmd runs cmd, capturing its stdout/stderr and exit code the same way
// the other e2e helpers do, so sandbox/docker invocations produce a result.
func captureCmd(t *testing.T, cmd *exec.Cmd) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	exitCode := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("run %v: %v", cmd.Args, err)
		}
	}
	return result{stdout: stdout.String(), stderr: stderr.String(), exitCode: exitCode}
}

// resolvedTempDir returns a fresh temp dir with symlinks resolved so a sandbox
// profile that denies writes by canonical path matches the paths bgx actually
// touches (macOS /tmp is a symlink to /private/tmp).
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bgx-fs-")
	if err != nil {
		t.Fatalf("make temp dir: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(resolved) })
	return resolved
}

// accessSandbox wraps a bgx invocation so that a chosen set of directories is
// made unwritable, exercising bgx's move-to-next-fallback behavior.
type accessSandbox struct {
	kind string // "seatbelt" or "bwrap"
}

// detectAccessSandbox returns the platform's sandbox, skipping the test when
// none is installed or usable in this environment.
func detectAccessSandbox(t *testing.T) accessSandbox {
	t.Helper()
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("sandbox-exec"); err == nil {
			s := accessSandbox{kind: "seatbelt"}
			if s.works() {
				return s
			}
		}
	case "linux":
		if _, err := exec.LookPath("bwrap"); err == nil {
			s := accessSandbox{kind: "bwrap"}
			if s.works() {
				return s
			}
		}
	}
	t.Skip("no usable access-control sandbox (seatbelt/bubblewrap) available")
	return accessSandbox{}
}

// works probes the sandbox with a trivial command so environments that ship the
// tool but can't run it (e.g. bubblewrap without user namespaces) are skipped
// rather than reported as failures.
func (s accessSandbox) works() bool {
	prog, err := exec.LookPath("true")
	if err != nil {
		prog = "/usr/bin/true"
	}
	return s.command("", nil, prog).Run() == nil
}

// command builds an *exec.Cmd running prog+args under the sandbox with each dir
// in denied made read-only/unwritable.
func (s accessSandbox) command(workDir string, denied []string, prog string, args ...string) *exec.Cmd {
	switch s.kind {
	case "seatbelt":
		full := append([]string{"-p", seatbeltProfile(denied), prog}, args...)
		cmd := exec.Command("sandbox-exec", full...)
		cmd.Dir = workDir
		return cmd
	case "bwrap":
		argv := []string{"--dev-bind", "/", "/"}
		for _, d := range denied {
			argv = append(argv, "--ro-bind", d, d)
		}
		if workDir != "" {
			argv = append(argv, "--chdir", workDir)
		}
		argv = append(argv, prog)
		argv = append(argv, args...)
		cmd := exec.Command("bwrap", argv...)
		cmd.Dir = workDir
		return cmd
	default:
		return exec.Command(prog, args...)
	}
}

// bgx runs the built binary under the sandbox with the given environment.
func (s accessSandbox) bgx(t *testing.T, workDir string, denied, env []string, args ...string) result {
	t.Helper()
	cmd := s.command(workDir, denied, binPath, args...)
	cmd.Env = append(os.Environ(), env...)
	return captureCmd(t, cmd)
}

// seatbeltProfile denies file writes beneath each directory while allowing
// everything else, so bgx can still exec, read, and write to the fallback dirs.
func seatbeltProfile(denied []string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	for _, d := range denied {
		fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", d)
	}
	return b.String()
}

// TestRuntimeDirAccessDeniedFallsBack verifies that an unwritable
// $XDG_RUNTIME_DIR causes bgx to advance to a later fallback, log a clear
// $XDG_RUNTIME_DIR notice to stderr, echo the same metadata in the run JSON,
// and still succeed.
func TestRuntimeDirAccessDeniedFallsBack(t *testing.T) {
	sb := detectAccessSandbox(t)
	root := resolvedTempDir(t)
	xdg := filepath.Join(root, "xdg")
	home := filepath.Join(root, "home")
	tmp := filepath.Join(root, "tmp")
	for _, d := range []string{xdg, home, tmp} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	env := []string{
		"XDG_RUNTIME_DIR=" + xdg,
		"HOME=" + home,
		"TMPDIR=" + tmp,
	}
	res := sb.bgx(t, tmp, []string{xdg, home}, env, "run", "fs-fallback", "echo", "hello")
	if res.exitCode != 0 {
		t.Fatalf("run exit=%d stderr=%q stdout=%q", res.exitCode, res.stderr, res.stdout)
	}
	if !strings.Contains(res.stderr, "$XDG_RUNTIME_DIR") {
		t.Fatalf("stderr missing $XDG_RUNTIME_DIR fallback notice: %q", res.stderr)
	}

	m := decodeJSON(t, res.stdout)
	fb, ok := m["fallback"].(string)
	if !ok || !strings.Contains(fb, "$XDG_RUNTIME_DIR") {
		t.Fatalf("run json missing $XDG_RUNTIME_DIR fallback notice: %v", m)
	}
	if sd, ok := m["socket_dir"].(string); !ok || !strings.HasPrefix(sd, tmp) {
		t.Fatalf("socket_dir = %v, want under %s", m["socket_dir"], tmp)
	}
	if rd, ok := m["retention_dir"].(string); !ok || !strings.HasPrefix(rd, tmp) {
		t.Fatalf("retention_dir = %v, want under %s", m["retention_dir"], tmp)
	}
}

// TestAllFallbacksDeniedReportsError verifies that when every candidate
// directory is unwritable, bgx exits non-zero with a clear all-fallbacks-failed
// JSON error instead of silently degrading.
func TestAllFallbacksDeniedReportsError(t *testing.T) {
	sb := detectAccessSandbox(t)
	root := resolvedTempDir(t)

	env := []string{
		"XDG_RUNTIME_DIR=" + filepath.Join(root, "xdg"),
		"HOME=" + filepath.Join(root, "home"),
		"TMPDIR=" + filepath.Join(root, "tmp"),
	}
	res := sb.bgx(t, root, []string{root}, env, "run", "fs-fail", "echo", "hello")
	if res.exitCode == 0 {
		t.Fatalf("run succeeded despite all fallbacks denied: stdout=%q stderr=%q", res.stdout, res.stderr)
	}

	m := decodeJSON(t, res.stdout)
	e, ok := m["error"].(string)
	if !ok || !strings.Contains(e, "all base directory fallbacks failed") {
		t.Fatalf("run json missing all-fallbacks error: %v", m)
	}
}

// TestDockerCreatesMissingDirs verifies that in an image where the target
// directories do not yet exist, bgx creates them idempotently and a run+info
// round-trip works. It requires a Linux bgx binary (so the host binary runs in
// the Linux container) and a usable docker daemon.
func TestDockerCreatesMissingDirs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("docker round-trip requires a linux bgx binary")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	if out, err := exec.Command("docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker not usable: %v: %s", err, out)
	}
	const image = "debian:stable-slim"
	if out, err := exec.Command("docker", "pull", image).CombinedOutput(); err != nil {
		t.Skipf("cannot pull %s: %v: %s", image, err, out)
	}

	script := `set -eu
test ! -e /xdgrt
bgx run demo echo hello >/dev/null
test -d /xdgrt/bgx/run
bgx wait demo >/dev/null
i=0
while [ "$i" -lt 50 ]; do
  out=$(bgx info demo)
  case "$out" in
    *'"running":false'*) printf '%s' "$out"; exit 0 ;;
  esac
  i=$((i + 1))
  sleep 0.1
done
printf '%s' "$out"
exit 1
`
	cmd := exec.Command("docker", "run", "--rm",
		"-e", "XDG_RUNTIME_DIR=/xdgrt",
		"-v", binPath+":/usr/local/bin/bgx:ro",
		image, "sh", "-c", script,
	)
	res := captureCmd(t, cmd)
	if res.exitCode != 0 {
		t.Fatalf("docker round-trip exit=%d stdout=%q stderr=%q", res.exitCode, res.stdout, res.stderr)
	}

	m := decodeJSON(t, res.stdout)
	if m["exists"] != true {
		t.Fatalf("info exists = %v, want true; stdout=%q", m["exists"], res.stdout)
	}
	if m["running"] != false {
		t.Fatalf("info running = %v, want false; stdout=%q", m["running"], res.stdout)
	}
	if m["id"] != "demo" {
		t.Fatalf("info id = %v, want demo; stdout=%q", m["id"], res.stdout)
	}
}
