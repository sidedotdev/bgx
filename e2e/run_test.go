package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sidedotdev/bgx/daemon"
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
	t.Cleanup(func() {
		if err := removeRunDir(dir); err != nil {
			t.Errorf("remove run dir: %v", err)
		}
	})
	return dir
}

func removeRunDir(dir string) error {
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for {
		if err := os.RemoveAll(dir); err != nil {
			lastErr = err
		} else {
			_, err := os.Stat(dir)
			switch {
			case errors.Is(err, os.ErrNotExist):
				return nil
			case err != nil:
				lastErr = err
			default:
				lastErr = fmt.Errorf("%s still exists after removal", dir)
			}
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// bgxIn runs the built binary with an isolated socket/retention environment so
// concurrent tests and repeated runs don't collide on session ids.
func bgxIn(t *testing.T, dir string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Env = append(os.Environ(),
		"XDG_RUNTIME_DIR="+dir,
		"XDG_STATE_HOME="+dir,
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

// decodeJSON parses a command's output (stdout or stderr) as a single JSON
// object.
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
		if res.exitCode != 0 {
			t.Fatalf(
				"info for session %q exited with code %d; stdout=%q stderr=%q",
				id,
				res.exitCode,
				res.stdout,
				res.stderr,
			)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(res.stdout), &m); err != nil {
			t.Fatalf(
				"invalid info JSON for session %q: %q: %v; exit code=%d stderr=%q",
				id,
				res.stdout,
				err,
				res.exitCode,
				res.stderr,
			)
		}
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

func TestRunSurfacesExecFailure(t *testing.T) {
	dir := runDir(t)

	res := bgxIn(t, dir, "run", "badexec", "no-such-command-xyz")
	if res.exitCode == 0 {
		t.Fatalf("run of missing command succeeded, want failure; stdout=%q", res.stdout)
	}
	m := decodeJSON(t, res.stderr)
	if _, ok := m["error"].(string); !ok {
		t.Fatalf("run stderr = %q, want JSON error", res.stderr)
	}
	if m["code"] != "startup_failed" {
		t.Fatalf("run error code = %v, want startup_failed", m["code"])
	}

	info := decodeJSON(t, bgxIn(t, dir, "info", "badexec").stdout)
	if info["exists"] != true {
		t.Fatalf("info exists = %v, want true; stdout=%q", info["exists"], info)
	}
	if _, ok := info["error"].(string); !ok {
		t.Fatalf("info missing error field: %v", info)
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
	m := decodeJSON(t, again.stderr)
	if _, ok := m["error"].(string); !ok {
		t.Fatalf("duplicate run stderr = %q, want JSON error", again.stderr)
	}
	if m["code"] != "already_exists" {
		t.Fatalf("duplicate run error code = %v, want already_exists", m["code"])
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
	m := decodeJSON(t, over.stderr)
	if _, ok := m["error"].(string); !ok {
		t.Fatalf("over-limit run stderr = %q, want JSON error", over.stderr)
	}
	if m["code"] != "concurrency_limit" {
		t.Fatalf("over-limit error code = %v, want concurrency_limit", m["code"])
	}
	if m["source"] != "bgx" {
		t.Fatalf("over-limit error source = %v, want %q", m["source"], "bgx")
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
			t.Cleanup(func() {
				res := bgxIn(t, dir, "kill", id)
				if res.exitCode != 0 {
					t.Errorf("kill %q exit code = %d, stderr=%q", id, res.exitCode, res.stderr)
					return
				}
				waitEnded(t, dir, id)
			})
		}
	}
	if succeeded != limit {
		t.Fatalf("concurrent runs succeeded = %d, want %d (cap must be enforced atomically)", succeeded, limit)
	}
}

// FuzzEndedSessionPersistence varies session timing, retained output, exit
// status, and namespace shape while requiring every observable representation
// of an ended session to describe the same complete lifecycle.
func FuzzEndedSessionPersistence(f *testing.F) {
	f.Add(uint16(0), uint32(0), uint32(0), uint8(0), false, uint8(0), uint8(0))
	f.Add(uint16(1), uint32(64), uint32(32), uint8(3), true, uint8(1), uint8(2))
	f.Add(uint16(31), uint32(65537), uint32(32769), uint8(125), true, uint8(25), uint8(25))
	f.Add(uint16(195), uint32(1<<20), uint32((1<<19)+1), uint8(3), false, uint8(7), uint8(19))

	f.Fuzz(func(t *testing.T, rawDelayMS uint16, rawOutputSize, rawSplit uint32, rawExitCode uint8, namespaced bool, rawFirst, rawSecond uint8) {
		delayMS := int(rawDelayMS % 201)
		outputSize := int(rawOutputSize % ((1 << 20) + 1))
		split := int(rawSplit % uint32(outputSize+1))
		exitCode := int(rawExitCode % 126)
		firstByte := byte('a' + rawFirst%26)
		secondByte := byte('A' + rawSecond%26)
		id := "fuzz"
		if namespaced {
			id = "fuzz/session"
		}

		dir := runDir(t)
		retentionDir := filepath.Join(dir, "bgx", "ended")
		recordPath := daemon.RecordPath(retentionDir, id)
		historyPath := daemon.HistoryPath(retentionDir, id)
		type persistedSession struct {
			info    daemon.Info
			history []byte
			err     error
		}
		persisted := make(chan persistedSession, 1)
		stopObserver := make(chan struct{})
		defer close(stopObserver)
		go func() {
			for {
				data, err := os.ReadFile(recordPath)
				if err == nil {
					var info daemon.Info
					if json.Unmarshal(data, &info) == nil {
						history, historyErr := os.ReadFile(historyPath)
						persisted <- persistedSession{info: info, history: history, err: historyErr}
						return
					}
				} else if !os.IsNotExist(err) {
					persisted <- persistedSession{err: fmt.Errorf("read ended record: %w", err)}
					return
				}
				select {
				case <-stopObserver:
					return
				default:
				}
			}
		}()

		var outputCommands []string
		if split > 0 {
			outputCommands = append(outputCommands,
				fmt.Sprintf("dd if=/dev/zero bs=%d count=1 2>/dev/null | LC_ALL=C tr '\\0' %c", split, firstByte))
		}
		if remaining := outputSize - split; remaining > 0 {
			outputCommands = append(outputCommands,
				fmt.Sprintf("dd if=/dev/zero bs=%d count=1 2>/dev/null | LC_ALL=C tr '\\0' %c", remaining, secondByte))
		}
		outputCommand := strings.Join(outputCommands, "; ")
		if outputCommand == "" {
			outputCommand = ":"
		}
		delay := strconv.FormatFloat(float64(delayMS)/1000, 'f', -1, 64)
		script := fmt.Sprintf("%s; sleep %s; exit %d", outputCommand, delay, exitCode)
		res := bgxIn(t, dir, "run", id, "sh", "-c", script)
		if res.exitCode != 0 {
			t.Fatalf("run exit code = %d, want 0; delay=%dms output=%d command_exit=%d namespace=%t stdout=%q stderr=%q",
				res.exitCode, delayMS, outputSize, exitCode, namespaced, res.stdout, res.stderr)
		}
		started := decodeJSON(t, res.stdout)
		if errMsg, ok := started["error"].(string); ok {
			t.Fatalf("run reported error %q; delay=%dms output=%d command_exit=%d namespace=%t",
				errMsg, delayMS, outputSize, exitCode, namespaced)
		}
		if started["id"] != id {
			t.Fatalf("run id = %v, want %s", started["id"], id)
		}

		var stored persistedSession
		select {
		case stored = <-persisted:
		case <-time.After(5 * time.Second):
			t.Fatalf("ended session was not persisted; delay=%dms output=%d command_exit=%d namespace=%t",
				delayMS, outputSize, exitCode, namespaced)
		}
		if stored.err != nil {
			t.Fatalf("persisted session artifacts are inconsistent: %v; delay=%dms output=%d command_exit=%d namespace=%t",
				stored.err, delayMS, outputSize, exitCode, namespaced)
		}

		expectedHistory := bytes.Repeat([]byte{firstByte}, split)
		expectedHistory = append(expectedHistory, bytes.Repeat([]byte{secondByte}, outputSize-split)...)
		if stored.info.ID != id {
			t.Errorf("persisted id = %q, want %q", stored.info.ID, id)
		}
		if stored.info.Running {
			t.Error("persisted session is still marked running")
		}
		if stored.info.EndedAt == nil {
			t.Error("persisted session has no ended_at")
		}
		if stored.info.ExitCode == nil || *stored.info.ExitCode != exitCode {
			t.Errorf("persisted exit_code = %v, want %d", stored.info.ExitCode, exitCode)
		}
		if stored.info.OutputBytes != int64(outputSize) {
			t.Errorf("persisted output_bytes = %d, want %d", stored.info.OutputBytes, outputSize)
		}
		if !bytes.Equal(stored.history, expectedHistory) {
			t.Errorf("persisted history length/content mismatch: got %d bytes, want %d", len(stored.history), len(expectedHistory))
		}

		info := waitEnded(t, dir, id)
		if info["id"] != id || info["running"] != false || info["exit_code"] != float64(exitCode) {
			t.Errorf("CLI info disagrees with persisted lifecycle: %v", info)
		}
		if info["output_bytes"] != float64(stored.info.OutputBytes) {
			t.Errorf("CLI output_bytes = %v, persisted output_bytes = %d", info["output_bytes"], stored.info.OutputBytes)
		}

		history := bgxIn(t, dir, "history", id)
		if history.exitCode != 0 {
			t.Fatalf("history exit code = %d; stderr=%q", history.exitCode, history.stderr)
		}
		if !bytes.Equal([]byte(history.stdout), stored.history) {
			t.Errorf("CLI history disagrees with persisted history: got %d bytes, want %d", len(history.stdout), len(stored.history))
		}
	})
}

// TestRunFailsPromptlyWhenDaemonExitsBeforeStartup proves that a daemon which
// dies during startup surfaces as an actionable run error well before the
// readiness timeout, honoring the constraint that a daemon must outlive its
// client.
func TestRunFailsPromptlyWhenDaemonExitsBeforeStartup(t *testing.T) {
	dir := runDir(t)

	// Occupy the session's socket path with a non-empty directory so the daemon
	// cannot bind its listener and exits during startup without persisting a
	// record.
	sockPath := filepath.Join(dir, "bgx", "run", "startupfail.sock")
	if err := os.MkdirAll(filepath.Join(sockPath, "block"), 0o700); err != nil {
		t.Fatalf("occupy socket path: %v", err)
	}

	start := time.Now()
	res := bgxIn(t, dir, "run", "startupfail", "echo", "hi")
	elapsed := time.Since(start)

	if res.exitCode == 0 {
		t.Fatalf("run succeeded despite daemon startup failure; stdout=%q", res.stdout)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("run took %s to fail, want prompt failure well under the readiness timeout", elapsed)
	}
	m := decodeJSON(t, res.stderr)
	errMsg, ok := m["error"].(string)
	if !ok {
		t.Fatalf("run stderr = %q, want JSON error", res.stderr)
	}
	// The message must identify the startup failure specifically, so an
	// unrelated early validation error can't satisfy the test, and it must not
	// be the readiness timeout, proving the daemon's exit was detected promptly.
	if !strings.Contains(errMsg, "daemon exited before startup completed") {
		t.Fatalf("run error = %q, want it to mention the daemon exiting before startup completed", errMsg)
	}
	if strings.Contains(errMsg, "timed out") {
		t.Fatalf("run error = %q, want a prompt startup failure, not a readiness timeout", errMsg)
	}
}
