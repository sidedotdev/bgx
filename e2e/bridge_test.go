package e2e

// These tests cover remote-session bridging: `bgx bridge` must forward the raw
// socket protocol verbatim over its stdio, and `bgx attach --via <cmd...>`
// (with --ssh as sugar) must attach through such a transport unchanged.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/sidedotdev/bgx/daemon"
)

// TestBridgeForwardsAttachProtocolVerbatim drives the raw attach protocol over
// `bgx bridge` stdio exactly as a remote attach client would: the JSON
// handshake and the tagged frames must pass through untouched both ways.
func TestBridgeForwardsAttachProtocolVerbatim(t *testing.T) {
	dir := runDir(t)
	if res := bgxIn(t, dir, "run", "br", "cat"); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}

	cmd := exec.Command(binPath, "bridge", "br")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "XDG_STATE_HOME="+dir, "TMPDIR="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bridge: %v", err)
	}
	watchdogErr := make(chan error, 1)
	watchdog := time.AfterFunc(30*time.Second, func() {
		watchdogErr <- cmd.Process.Kill()
	})
	defer func() {
		if !watchdog.Stop() {
			if err := <-watchdogErr; err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("kill timed-out bridge: %v", err)
			}
		}
	}()

	if _, err := stdin.Write([]byte("{\"op\":\"attach\"}\n")); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	br := bufio.NewReader(stdout)
	line, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read handshake response: %v, stderr=%q", err, stderr.String())
	}
	var resp daemon.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode handshake response %q: %v", line, err)
	}
	if !resp.OK {
		t.Fatalf("handshake refused: %q", line)
	}

	if err := daemon.WriteFrame(stdin, daemon.FrameInput, []byte("marco\r")); err != nil {
		t.Fatalf("write input frame: %v", err)
	}
	var output []byte
	for !strings.Contains(string(output), "marco") {
		tag, payload, err := daemon.ReadFrame(br)
		if err != nil {
			t.Fatalf("read frame: %v (output=%q, stderr=%q)", err, output, stderr.String())
		}
		if tag == daemon.FrameOutput || tag == daemon.FrameResync {
			output = append(output, payload...)
		}
	}

	if err := daemon.WriteFrame(stdin, daemon.FrameDetach, nil); err != nil {
		t.Fatalf("write detach frame: %v", err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatalf("close bridge stdin: %v", err)
	}
	if _, err := io.Copy(io.Discard, br); err != nil {
		t.Fatalf("drain bridge stdout: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("bridge exit: %v, stderr=%q", err, stderr.String())
	}

	// Detaching through the bridge must leave the session running.
	res := bgxIn(t, dir, "info", "br")
	if res.exitCode != 0 {
		t.Fatalf("info exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	info := decodeJSON(t, res.stdout)
	if info["running"] != true {
		t.Fatalf("session not running after detach: %q", res.stdout)
	}
}

func TestBridgeReportsEndedAndMissingSessions(t *testing.T) {
	dir := runDir(t)

	res := bgxIn(t, dir, "bridge", "ghost")
	if res.exitCode == 0 {
		t.Fatalf("bridge succeeded for missing session; stdout=%q", res.stdout)
	}
	errObj := decodeJSON(t, res.stderr)
	if errObj["code"] != "session_not_found" {
		t.Fatalf("code = %v, want session_not_found; stderr=%q", errObj["code"], res.stderr)
	}

	if res := bgxIn(t, dir, "run", "brdone", "echo", "hi"); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	waitEnded(t, dir, "brdone")
	res = bgxIn(t, dir, "bridge", "brdone")
	if res.exitCode == 0 {
		t.Fatalf("bridge succeeded for ended session; stdout=%q", res.stdout)
	}
	errObj = decodeJSON(t, res.stderr)
	if errObj["code"] != "session_ended" {
		t.Fatalf("code = %v, want session_ended; stderr=%q", errObj["code"], res.stderr)
	}
}

// wrapBGXOnPath returns a directory holding a `bgx` symlink to the built
// binary, so a transport command resolving "bgx" via PATH stands in for ssh
// resolving bgx on a remote host.
func wrapBGXOnPath(t *testing.T) string {
	t.Helper()
	wrapDir := t.TempDir()
	if err := os.Symlink(binPath, filepath.Join(wrapDir, "bgx")); err != nil {
		t.Fatalf("symlink bgx: %v", err)
	}
	return wrapDir
}

// driveInteractiveAttach runs an attach command under a pty. It first waits
// for marker (pre-seeded session output) to replay, which proves the attach
// handshake completed and raw mode is active — typing earlier would only be
// echoed by the pty line discipline and ctrl+\ would raise SIGQUIT. It then
// sends input, waits for want to render, and detaches with ctrl+\, asserting
// a clean exit.
func driveInteractiveAttach(t *testing.T, cmd *exec.Cmd, marker, input, want string) {
	t.Helper()
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("pty start: %v", err)
	}
	defer closePTY(t, ptmx)

	var mu sync.Mutex
	var got []byte
	readDone := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		defer close(readDone)
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				mu.Lock()
				got = append(got, buf[:n]...)
				mu.Unlock()
			}
			if err != nil {
				if !expectedPTYReadError(err) {
					readErr <- err
				}
				return
			}
		}
	}()
	output := func() string {
		mu.Lock()
		defer mu.Unlock()
		return string(got)
	}
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !strings.Contains(output(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %q; output=%q", want, output())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	waitFor(marker)
	if _, err := ptmx.Write([]byte(input)); err != nil {
		t.Fatalf("write input: %v", err)
	}
	waitFor(want)

	if _, err := ptmx.Write([]byte{0x1c}); err != nil {
		t.Fatalf("write detach: %v", err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("attach exit: %v; output=%q", err, output())
		}
	case <-time.After(15 * time.Second):
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("kill attach after timeout: %v; output=%q", err, output())
		}
		if err := <-waitErr; err == nil {
			t.Fatalf("attach did not exit after detach; output=%q", output())
		} else {
			t.Fatalf("attach did not exit after detach: %v; output=%q", err, output())
		}
	}
	<-readDone
	select {
	case err := <-readErr:
		t.Fatalf("read attach pty: %v", err)
	default:
	}
}

// expectRunning asserts the session is still running after a remote detach.
func expectRunning(t *testing.T, dir, id string) {
	t.Helper()
	res := bgxIn(t, dir, "info", id)
	if res.exitCode != 0 {
		t.Fatalf("info exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	info := decodeJSON(t, res.stdout)
	if info["running"] != true {
		t.Fatalf("session not running after detach: %q", res.stdout)
	}
}

// TestAttachViaTransportBridgesRemoteSession exercises attach --via end to end
// with a local stand-in for ssh: the transport command must have `bgx bridge
// <id>` appended and the interactive attach must work over its stdio, from the
// snapshot replay through input echo to ctrl+\ detach.
func TestAttachViaTransportBridgesRemoteSession(t *testing.T) {
	dir := runDir(t)
	if res := bgxIn(t, dir, "run", "viasess", "cat"); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	if res := bgxIn(t, dir, "send", "viasess", "viamarker"); res.exitCode != 0 {
		t.Fatalf("send exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	historyContains(t, dir, "viasess", "viamarker")
	wrapDir := wrapBGXOnPath(t)

	cmd := exec.Command(binPath, "attach", "viasess",
		"--via", "env", "PATH="+wrapDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "XDG_STATE_HOME="+dir, "TMPDIR="+dir)
	driveInteractiveAttach(t, cmd, "viamarker", "polo\r", "polo")
	expectRunning(t, dir, "viasess")
}

// TestAttachSSHExpandsToTransportCommand verifies --ssh is sugar for --via ssh
// <host>: attach must invoke `ssh <host> bgx bridge <id>`, here against a fake
// ssh that records the host and execs the rest of its arguments locally.
func TestAttachSSHExpandsToTransportCommand(t *testing.T) {
	dir := runDir(t)
	if res := bgxIn(t, dir, "run", "viassh", "cat"); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	if res := bgxIn(t, dir, "send", "viassh", "sshmarker"); res.exitCode != 0 {
		t.Fatalf("send exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	historyContains(t, dir, "viassh", "sshmarker")
	wrapDir := wrapBGXOnPath(t)
	hostRecord := filepath.Join(wrapDir, "host")
	script := "#!/bin/sh\nprintf '%s' \"$1\" > " + hostRecord + "\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(wrapDir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}

	cmd := exec.Command(binPath, "attach", "--ssh", "remotehost", "viassh")
	cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+dir, "XDG_STATE_HOME="+dir, "TMPDIR="+dir,
		"PATH="+wrapDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	driveInteractiveAttach(t, cmd, "sshmarker", "ping\r", "ping")
	expectRunning(t, dir, "viassh")

	host, err := os.ReadFile(hostRecord)
	if err != nil {
		t.Fatalf("fake ssh never ran: %v", err)
	}
	if string(host) != "remotehost" {
		t.Fatalf("ssh host = %q, want remotehost", host)
	}
}

// assertSingleJSONError enforces the single-JSON-error contract: exactly one
// JSON error line on stderr, nothing on stdout, with the wanted code.
func assertSingleJSONError(t *testing.T, res result, wantCode, wantMsg string) {
	t.Helper()
	if res.exitCode == 0 {
		t.Fatalf("command succeeded; stdout=%q", res.stdout)
	}
	if res.stdout != "" {
		t.Fatalf("error leaked to stdout: %q", res.stdout)
	}
	lines := strings.Split(strings.TrimSpace(res.stderr), "\n")
	if len(lines) != 1 {
		t.Fatalf("stderr has %d lines, want a single JSON error: %q", len(lines), res.stderr)
	}
	errObj := decodeJSON(t, lines[0])
	if errObj["code"] != wantCode {
		t.Fatalf("code = %v, want %s; stderr=%q", errObj["code"], wantCode, res.stderr)
	}
	msg, ok := errObj["error"].(string)
	if !ok {
		t.Fatalf("error = %T, want string; stderr=%q", errObj["error"], res.stderr)
	}
	if !strings.Contains(msg, wantMsg) {
		t.Fatalf("error = %q, want mention of %q", msg, wantMsg)
	}
}

// TestAttachViaReportsRemoteMissingAndEndedSessions verifies a remote bridge
// failure surfaces locally as exactly one JSON error carrying the remote
// session_not_found/session_ended code, not a duplicate attach_failed error.
func TestAttachViaReportsRemoteMissingAndEndedSessions(t *testing.T) {
	dir := runDir(t)
	wrapDir := wrapBGXOnPath(t)
	viaArgs := []string{"--via", "env", "PATH=" + wrapDir + string(os.PathListSeparator) + os.Getenv("PATH")}

	res := bgxIn(t, dir, append([]string{"attach", "ghost"}, viaArgs...)...)
	assertSingleJSONError(t, res, "session_not_found", "does not exist")

	if res := bgxIn(t, dir, "run", "viadone", "echo", "hi"); res.exitCode != 0 {
		t.Fatalf("run exit = %d, stderr=%q", res.exitCode, res.stderr)
	}
	waitEnded(t, dir, "viadone")
	res = bgxIn(t, dir, append([]string{"attach", "viadone"}, viaArgs...)...)
	assertSingleJSONError(t, res, "session_ended", "already ended")
}

func TestAttachRejectsBothSSHAndVia(t *testing.T) {
	dir := runDir(t)
	res := bgxIn(t, dir, "attach", "x", "--ssh", "host", "--via", "true")
	if res.exitCode == 0 {
		t.Fatalf("attach succeeded with both --ssh and --via; stdout=%q", res.stdout)
	}
	errObj := decodeJSON(t, res.stderr)
	if errObj["code"] != "invalid_argument" {
		t.Fatalf("code = %v, want invalid_argument; stderr=%q", errObj["code"], res.stderr)
	}
	msg, ok := errObj["error"].(string)
	if !ok {
		t.Fatalf("error = %T, want string; stderr=%q", errObj["error"], res.stderr)
	}
	if !strings.Contains(msg, "--ssh") || !strings.Contains(msg, "--via") {
		t.Fatalf("error = %q, want mention of both --ssh and --via", msg)
	}
}
