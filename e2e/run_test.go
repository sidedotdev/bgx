package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
)

// runDir creates a short-lived directory under /tmp used as both the XDG
// runtime dir and TMPDIR for a test. /tmp is used (rather than t.TempDir,
// which lives under the long macOS /var/folders path) to keep session socket
// paths within the unix sun_path length limit.
func runDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "bgx-e2e-")
	if err != nil {
		t.Fatalf("make run dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// bgxIn runs the built binary with an isolated socket/retention environment so
// concurrent tests and repeated runs don't collide on session ids.
func bgxIn(t *testing.T, dir string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Env = append(os.Environ(),
		"XDG_RUNTIME_DIR="+dir,
		"TMPDIR="+dir,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("failed to run bgx %v: %v", args, err)
		}
	}
	return result{stdout: stdout.String(), stderr: stderr.String(), exitCode: exitCode}
}

// decodeJSON parses a command's stdout as a single JSON object.
func decodeJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("invalid JSON %q: %v", s, err)
	}
	return m
}

// waitEnded polls info until the session has a persisted ended record, ensuring
// later assertions observe a consistent post-exit state.
func waitEnded(t *testing.T, dir, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		res := bgxIn(t, dir, "info", id)
		m := decodeJSON(t, res.stdout)
		if m["exists"] == true && m["running"] == false {
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %q did not reach ended state; last info: %v", id, m)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunWaitReturnsZeroExitCode(t *testing.T) {
	dir := runDir(t)

	res := bgxIn(t, dir, "run", "echo-ok", "echo", "hello")
	if res.exitCode != 0 {
		t.Fatalf("run exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}
	started := decodeJSON(t, res.stdout)
	if started["id"] != "echo-ok" {
		t.Fatalf("run id = %v, want echo-ok", started["id"])
	}
	if pid, ok := started["pid"].(float64); !ok || pid <= 0 {
		t.Fatalf("run pid = %v, want positive number", started["pid"])
	}
	if _, ok := started["started_at"].(string); !ok {
		t.Fatalf("run started_at = %v, want timestamp string", started["started_at"])
	}

	wait := bgxIn(t, dir, "wait", "echo-ok")
	if wait.exitCode != 0 {
		t.Fatalf("wait exit code = %d, stderr=%q", wait.exitCode, wait.stderr)
	}
	m := decodeJSON(t, wait.stdout)
	if m["exit_code"] != float64(0) {
		t.Fatalf("wait exit_code = %v, want 0", m["exit_code"])
	}
}

func TestRunWaitReturnsNonzeroExitCode(t *testing.T) {
	dir := runDir(t)

	res := bgxIn(t, dir, "run", "exit3", "sh", "-c", "exit 3")
	if res.exitCode != 0 {
		t.Fatalf("run exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}

	wait := bgxIn(t, dir, "wait", "exit3")
	if wait.exitCode != 3 {
		t.Fatalf("wait exit code = %d, want 3, stderr=%q", wait.exitCode, wait.stderr)
	}
	m := decodeJSON(t, wait.stdout)
	if m["exit_code"] != float64(3) {
		t.Fatalf("wait exit_code = %v, want 3", m["exit_code"])
	}
}

func TestInfoReportsMetadataAndOutput(t *testing.T) {
	dir := runDir(t)

	res := bgxIn(t, dir, "run", "--metadata", "team=infra", "info-meta", "echo", "hello")
	if res.exitCode != 0 {
		t.Fatalf("run exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}

	m := waitEnded(t, dir, "info-meta")
	md, ok := m["metadata"].(map[string]any)
	if !ok || md["team"] != "infra" {
		t.Fatalf("info metadata = %v, want team=infra", m["metadata"])
	}
	if s, ok := m["started_at"].(string); !ok || s == "" || s == "0001-01-01T00:00:00Z" {
		t.Fatalf("info started_at = %v, want real timestamp", m["started_at"])
	}
	if ob, ok := m["output_bytes"].(float64); !ok || ob <= 0 {
		t.Fatalf("info output_bytes = %v, want > 0", m["output_bytes"])
	}
}

func TestInfoMissingSessionReportsNotExists(t *testing.T) {
	dir := runDir(t)

	res := bgxIn(t, dir, "info", "nope")
	if res.exitCode != 0 {
		t.Fatalf("info exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}
	m := decodeJSON(t, res.stdout)
	if m["exists"] != false {
		t.Fatalf("info exists = %v, want false", m["exists"])
	}
}

func TestDuplicateIDRequiresOverwrite(t *testing.T) {
	dir := runDir(t)

	if res := bgxIn(t, dir, "run", "dup", "echo", "first"); res.exitCode != 0 {
		t.Fatalf("first run exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}
	waitEnded(t, dir, "dup")

	again := bgxIn(t, dir, "run", "dup", "echo", "second")
	if again.exitCode == 0 {
		t.Fatalf("duplicate run without --overwrite-id succeeded, want failure; stdout=%q", again.stdout)
	}
	m := decodeJSON(t, again.stdout)
	if _, ok := m["error"].(string); !ok {
		t.Fatalf("duplicate run output = %q, want JSON error", again.stdout)
	}

	overwrite := bgxIn(t, dir, "run", "--overwrite-id", "dup", "echo", "third")
	if overwrite.exitCode != 0 {
		t.Fatalf("overwrite run exit code = %d, stderr=%q", overwrite.exitCode, overwrite.stderr)
	}
	started := decodeJSON(t, overwrite.stdout)
	if started["id"] != "dup" {
		t.Fatalf("overwrite run id = %v, want dup", started["id"])
	}
}

func TestRunEnforcesNamespaceConcurrencyLimit(t *testing.T) {
	dir := runDir(t)

	for _, id := range []string{"ns/a", "ns/b"} {
		id := id
		res := bgxIn(t, dir, "run", "--concurrency", "2", id, "sleep", "30")
		if res.exitCode != 0 {
			t.Fatalf("run %s exit code = %d, stderr=%q", id, res.exitCode, res.stderr)
		}
		t.Cleanup(func() { bgxIn(t, dir, "kill", id) })
	}

	over := bgxIn(t, dir, "run", "--concurrency", "2", "ns/c", "sleep", "30")
	if over.exitCode == 0 {
		t.Cleanup(func() { bgxIn(t, dir, "kill", "ns/c") })
		t.Fatalf("run over limit succeeded, want failure; stdout=%q", over.stdout)
	}
	m := decodeJSON(t, over.stdout)
	if _, ok := m["error"].(string); !ok {
		t.Fatalf("over-limit run output = %q, want JSON error", over.stdout)
	}
	sessions, ok := m["sessions"].([]any)
	if !ok || len(sessions) != 2 {
		t.Fatalf("over-limit sessions = %v, want 2 active sessions", m["sessions"])
	}

	// A different namespace has its own independent budget.
	other := bgxIn(t, dir, "run", "--concurrency", "2", "other/a", "sleep", "30")
	if other.exitCode != 0 {
		t.Fatalf("run in separate namespace exit code = %d, stderr=%q", other.exitCode, other.stderr)
	}
	t.Cleanup(func() { bgxIn(t, dir, "kill", "other/a") })
}

// TestRunConcurrentInvocationsRespectLimit launches many simultaneous runs in
// one namespace and proves the cap is enforced atomically: exactly the limit
// number win, so a check-then-spawn race cannot exceed it.
func TestRunConcurrentInvocationsRespectLimit(t *testing.T) {
	dir := runDir(t)

	const limit = 1
	const attempts = 6
	results := make([]result, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("race/s%d", i)
			results[i] = bgxIn(t, dir, "run", "--concurrency", strconv.Itoa(limit), id, "sleep", "30")
		}()
	}
	wg.Wait()

	succeeded := 0
	for i := range results {
		if results[i].exitCode == 0 {
			succeeded++
			id := fmt.Sprintf("race/s%d", i)
			t.Cleanup(func() { bgxIn(t, dir, "kill", id) })
		}
	}
	if succeeded != limit {
		t.Fatalf("concurrent runs succeeded = %d, want %d (cap must be enforced atomically)", succeeded, limit)
	}
}
