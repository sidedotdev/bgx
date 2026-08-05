package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sidedotdev/bgx/daemon"
)

// historyContains polls a session's history until it contains want, returning
// the matched output or failing once the deadline passes.
func historyContains(t *testing.T, dir, id, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h := bgxIn(t, dir, "history", id)
		if strings.Contains(h.stdout, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("history for %q never contained %q; got %q", id, want, h.stdout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSendReachesPTYAndAppearsInHistory(t *testing.T) {
	dir := runDir(t)

	if res := bgxIn(t, dir, "run", "catsess", "cat"); res.exitCode != 0 {
		t.Fatalf("run exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}

	if res := bgxIn(t, dir, "send", "catsess", "ping"); res.exitCode != 0 {
		t.Fatalf("send exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}

	historyContains(t, dir, "catsess", "ping")

	if res := bgxIn(t, dir, "kill", "catsess"); res.exitCode != 0 {
		t.Fatalf("kill exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}
}

func TestKillStopsRunningSession(t *testing.T) {
	dir := runDir(t)

	if res := bgxIn(t, dir, "run", "sleepy", "sleep", "60"); res.exitCode != 0 {
		t.Fatalf("run exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}

	kill := bgxIn(t, dir, "kill", "sleepy")
	if kill.exitCode != 0 {
		t.Fatalf("kill exit code = %d, stderr=%q", kill.exitCode, kill.stderr)
	}
	km := decodeJSON(t, kill.stdout)
	if km["exists"] != true {
		t.Fatalf("kill result exists = %v, want true", km["exists"])
	}
	if km["running"] != false {
		t.Fatalf("kill result running = %v, want false", km["running"])
	}
	if km["killed"] != true {
		t.Fatalf("kill result killed = %v, want true", km["killed"])
	}

	wait := bgxIn(t, dir, "wait", "sleepy")
	if wait.exitCode == 0 {
		t.Fatalf("wait after kill exit code = %d, want nonzero", wait.exitCode)
	}

	m := waitEnded(t, dir, "sleepy")
	if m["killed"] != true {
		t.Fatalf("info killed = %v, want true", m["killed"])
	}
}

func TestHistoryKeepsHeadAndTailDiscardsMiddle(t *testing.T) {
	dir := runDir(t)

	const output = "ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"
	res := bgxIn(t, dir, "run",
		"--head-size", "8", "--tail-size", "8",
		"trunc", "sh", "-c", "printf '"+output+"'")
	if res.exitCode != 0 {
		t.Fatalf("run exit code = %d, stderr=%q", res.exitCode, res.stderr)
	}

	m := waitEnded(t, dir, "trunc")
	if ob, ok := m["output_bytes"].(float64); !ok || int(ob) != len(output) {
		t.Fatalf("info output_bytes = %v, want %d", m["output_bytes"], len(output))
	}

	// The discarded middle is demarcated by a blank line, a marker rule, a
	// "[...] truncated <N>" notice, another rule and a blank line, with a full
	// terminal reset (RIS) immediately before the retained tail so it renders
	// on a clean state.
	const rule = "────────────────────────────────────────"
	h := bgxIn(t, dir, "history", "trunc")
	discarded := len(output) - 16
	want := output[:8] +
		"\r\n\r\n" + rule + "\r\n" +
		fmt.Sprintf("[...] truncated %dB", discarded) +
		"\r\n" + rule + "\r\n\r\n" +
		"\x1bc" + output[len(output)-8:]
	if h.stdout != want {
		t.Fatalf("history = %q, want %q", h.stdout, want)
	}
}

func FuzzKilledSessionLifecycle(f *testing.F) {
	f.Add(uint16(0), false, false, false)
	f.Add(uint16(1), true, false, true)
	f.Add(uint16(49), false, true, true)
	f.Add(uint16(100), true, true, false)

	f.Fuzz(func(t *testing.T, rawKillDelayMS uint16, ignoreHUP, namespaced, diskStorage bool) {
		killDelay := time.Duration(rawKillDelayMS%101) * time.Millisecond
		id := "fuzz-kill"
		if namespaced {
			id = "fuzz-kill/session"
		}
		const prefix = "fuzz-kill-ready"

		dir := runDir(t)
		storagePath := filepath.Join(dir, "scrollback")
		args := []string{"run"}
		if diskStorage {
			args = append(args, "--storage", "disk", "--storage-path", storagePath)
		} else {
			args = append(args, "--storage", "memory")
		}
		signalAction := "exit 0"
		if ignoreHUP {
			signalAction = ""
		}
		script := fmt.Sprintf(
			"trap '%s' HUP; printf %s; i=0; while :; do printf '<%%08d>' \"$i\"; i=$((i+1)); sleep 0.01; done",
			signalAction,
			prefix,
		)
		args = append(args, id, "sh", "-c", script)

		run := bgxIn(t, dir, args...)
		if run.exitCode != 0 {
			t.Fatalf("run exit code = %d; stderr=%q", run.exitCode, run.stderr)
		}
		historyContains(t, dir, id, prefix)
		time.Sleep(killDelay)

		kill := bgxIn(t, dir, "kill", id)
		if kill.exitCode != 0 {
			t.Fatalf("kill exit code = %d; stdout=%q stderr=%q", kill.exitCode, kill.stdout, kill.stderr)
		}
		killInfo := decodeJSON(t, kill.stdout)
		if killInfo["id"] != id || killInfo["exists"] != true || killInfo["running"] != false || killInfo["killed"] != true {
			t.Errorf("kill result has inconsistent lifecycle state: %v", killInfo)
		}

		info := waitEnded(t, dir, id)
		if info["id"] != id || info["exists"] != true || info["running"] != false || info["killed"] != true {
			t.Errorf("info result has inconsistent lifecycle state: %v", info)
		}
		if killInfo["exit_code"] != info["exit_code"] {
			t.Errorf("kill exit_code = %v, info exit_code = %v", killInfo["exit_code"], info["exit_code"])
		}
		infoExitCode, ok := info["exit_code"].(float64)
		if !ok {
			t.Fatalf("info exit_code has unexpected type/value: %v", info["exit_code"])
		}

		wait := bgxIn(t, dir, "wait", id)
		if wait.exitCode != int(infoExitCode) {
			t.Errorf("wait process exit code = %d, info exit_code = %v; stdout=%q stderr=%q",
				wait.exitCode, info["exit_code"], wait.stdout, wait.stderr)
		}
		waitInfo := decodeJSON(t, wait.stdout)
		if waitInfo["id"] != id || waitInfo["exit_code"] != info["exit_code"] {
			t.Errorf("wait result disagrees with info: wait=%v info=%v", waitInfo, info)
		}

		retentionDir := filepath.Join(dir, "bgx", "ended")
		recordData, err := os.ReadFile(daemon.RecordPath(retentionDir, id))
		if err != nil {
			t.Fatalf("read persisted record: %v", err)
		}
		var persisted daemon.Info
		if err := json.Unmarshal(recordData, &persisted); err != nil {
			t.Fatalf("decode persisted record: %v", err)
		}
		if persisted.ID != id || persisted.Running || !persisted.Killed || persisted.EndedAt == nil || persisted.ExitCode == nil {
			t.Errorf("persisted record has inconsistent lifecycle state: %+v", persisted)
		}
		if float64(*persisted.ExitCode) != info["exit_code"] {
			t.Errorf("persisted exit_code = %d, info exit_code = %v", *persisted.ExitCode, info["exit_code"])
		}

		persistedHistory, err := os.ReadFile(daemon.HistoryPath(retentionDir, id))
		if err != nil {
			t.Fatalf("read persisted history: %v", err)
		}
		if persisted.OutputBytes != int64(len(persistedHistory)) {
			t.Errorf("persisted output_bytes = %d, history length = %d", persisted.OutputBytes, len(persistedHistory))
		}
		if info["output_bytes"] != float64(len(persistedHistory)) {
			t.Errorf("CLI output_bytes = %v, history length = %d", info["output_bytes"], len(persistedHistory))
		}

		history := bgxIn(t, dir, "history", id)
		if history.exitCode != 0 {
			t.Fatalf("history exit code = %d; stderr=%q", history.exitCode, history.stderr)
		}
		if !bytes.Equal([]byte(history.stdout), persistedHistory) {
			t.Errorf("CLI and persisted histories differ: cli=%d persisted=%d", len(history.stdout), len(persistedHistory))
		}
		if !bytes.Contains(persistedHistory, []byte(prefix)) {
			t.Errorf("persisted history does not contain readiness prefix: %q", persistedHistory)
		}

		if diskStorage {
			deadline := time.Now().Add(5 * time.Second)
			for {
				entries, err := os.ReadDir(storagePath)
				if err == nil && len(entries) == 0 {
					break
				}
				if err != nil && !os.IsNotExist(err) {
					t.Fatalf("read storage path: %v", err)
				}
				if time.Now().After(deadline) {
					t.Fatalf("disk scrollback storage was not cleaned up: entries=%v err=%v", entries, err)
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	})
}
