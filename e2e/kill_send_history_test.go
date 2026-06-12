package e2e

import (
	"strings"
	"testing"
	"time"
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

	h := bgxIn(t, dir, "history", "trunc")
	want := output[:8] + output[len(output)-8:]
	if h.stdout != want {
		t.Fatalf("history = %q, want %q", h.stdout, want)
	}
}
