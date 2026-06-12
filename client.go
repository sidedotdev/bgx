package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sidedotdev/bgx/daemon"
	cli "github.com/urfave/cli/v3"
)

// maxSocketPathLen is a conservative cap on a unix domain socket path length.
// Linux allows 108 and macOS 104 bytes for sun_path; the smaller bound keeps a
// given id portable across both.
const maxSocketPathLen = 104

// socketReadyTimeout bounds how long run waits for a freshly spawned daemon to
// bind its socket (or, for a very short-lived command, to persist a record).
const socketReadyTimeout = 5 * time.Second

// socketPath returns the unix domain socket path for a session id, encoding the
// id into a single safe filename component.
func socketPath(id string) string {
	return filepath.Join(socketDir(), url.QueryEscape(id)+".sock")
}

// infoResult is the JSON shape emitted by the info command: an existence flag
// plus, when present, the session's full metadata snapshot.
type infoResult struct {
	Exists bool `json:"exists"`
	*daemon.Info
}

// dialRequest sends a single JSON-line request to a session's socket and
// decodes the reply.
func dialRequest(id string, req daemon.Request) (daemon.Response, error) {
	conn, err := net.Dial("unix", socketPath(id))
	if err != nil {
		return daemon.Response{}, err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return daemon.Response{}, err
	}
	var resp daemon.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return daemon.Response{}, err
	}
	return resp, nil
}

// liveInfo queries a running session's daemon, reporting whether one answered.
func liveInfo(id string) (*daemon.Info, bool) {
	resp, err := dialRequest(id, daemon.Request{Op: "info"})
	if err != nil || !resp.OK || resp.Info == nil {
		return nil, false
	}
	return resp.Info, true
}

// endedRecord reads the persisted record for an ended session, if one exists.
func endedRecord(id string) (*daemon.Info, bool) {
	data, err := os.ReadFile(daemon.RecordPath(retentionDir(), id))
	if err != nil {
		return nil, false
	}
	var info daemon.Info
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, false
	}
	return &info, true
}

// failJSON prints a JSON error object and exits non-zero so failures stay
// machine-readable like every other command's output.
func failJSON(format string, args ...any) error {
	_ = printJSON(os.Stdout, map[string]string{"error": fmt.Sprintf(format, args...)})
	os.Exit(1)
	return nil
}

func runAction(_ context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) == 0 {
		return failJSON("run: an id is required")
	}
	id, command := args[0], args[1:]
	if id == "" {
		return failJSON("run: id must not be empty")
	}
	if len(command) == 0 {
		return failJSON("run: a command is required")
	}
	metadata := cmd.StringSlice("metadata")
	if _, err := parseMetadata(metadata); err != nil {
		return failJSON("run: %v", err)
	}
	if len(socketPath(id)) > maxSocketPathLen {
		return failJSON("run: socket path for id %q exceeds %d bytes", id, maxSocketPathLen)
	}

	if _, ok := liveInfo(id); ok {
		return failJSON("run: session %q is already running", id)
	}
	if _, ok := endedRecord(id); ok {
		if !cmd.Bool("overwrite-id") {
			return failJSON("run: session %q already exists; pass --overwrite-id to replace it", id)
		}
		os.Remove(daemon.RecordPath(retentionDir(), id))
		os.Remove(daemon.HistoryPath(retentionDir(), id))
	}

	if err := spawnDaemon(id, command, metadata, cmd); err != nil {
		return failJSON("run: %v", err)
	}

	info, err := waitForSession(id, socketReadyTimeout)
	if err != nil {
		return failJSON("run: %v", err)
	}
	return printJSON(os.Stdout, map[string]any{
		"id":         info.ID,
		"pid":        info.Pid,
		"started_at": info.StartedAt,
	})
}

// spawnDaemon re-execs the bgx binary's hidden __daemon subcommand in its own
// session with detached stdio so the session outlives this client.
func spawnDaemon(id string, command, metadata []string, cmd *cli.Command) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{
		"__daemon",
		"--id", id,
		"--socket", socketPath(id),
		"--retention-dir", retentionDir(),
	}
	if v := cmd.Int("head-size"); v > 0 {
		args = append(args, "--head-size", strconv.Itoa(v))
	}
	if v := cmd.Int("tail-size"); v > 0 {
		args = append(args, "--tail-size", strconv.Itoa(v))
	}
	if v := cmd.String("storage"); v != "" {
		args = append(args, "--storage", v)
	}
	if v := cmd.String("storage-path"); v != "" {
		args = append(args, "--storage-path", v)
	}
	for _, m := range metadata {
		args = append(args, "--metadata", m)
	}
	args = append(args, "--")
	args = append(args, command...)

	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()

	dc := exec.Command(exe, args...)
	dc.Stdin = devnull
	dc.Stdout = devnull
	dc.Stderr = devnull
	dc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return dc.Start()
}

// waitForSession blocks until a freshly spawned session answers on its socket
// or, for a command that already exited, has persisted a record, returning the
// resulting metadata snapshot.
func waitForSession(id string, timeout time.Duration) (*daemon.Info, error) {
	deadline := time.Now().Add(timeout)
	for {
		if info, ok := liveInfo(id); ok {
			return info, nil
		}
		if info, ok := endedRecord(id); ok {
			return info, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for session %q to start", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func infoAction(_ context.Context, cmd *cli.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON("info: an id is required")
	}
	if info, ok := liveInfo(id); ok {
		return printJSON(os.Stdout, infoResult{Exists: true, Info: info})
	}
	if info, ok := endedRecord(id); ok {
		return printJSON(os.Stdout, infoResult{Exists: true, Info: info})
	}
	return printJSON(os.Stdout, map[string]any{"id": id, "exists": false})
}

func waitAction(_ context.Context, cmd *cli.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON("wait: an id is required")
	}
	if resp, err := dialRequest(id, daemon.Request{Op: "wait"}); err == nil && resp.OK && resp.ExitCode != nil {
		return emitExit(id, *resp.ExitCode)
	}
	if info, ok := endedRecord(id); ok && info.ExitCode != nil {
		return emitExit(id, *info.ExitCode)
	}
	return failJSON("wait: session %q not found", id)
}

// emitExit prints the session's exit code as JSON and mirrors it as the bgx
// process exit status.
func emitExit(id string, code int) error {
	_ = printJSON(os.Stdout, map[string]any{"id": id, "exit_code": code})
	os.Exit(code)
	return nil
}

func killAction(_ context.Context, cmd *cli.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON("kill: an id is required")
	}
	if resp, err := dialRequest(id, daemon.Request{Op: "kill"}); err == nil && resp.OK && resp.Info != nil {
		return printJSON(os.Stdout, infoResult{Exists: true, Info: resp.Info})
	}
	if info, ok := endedRecord(id); ok {
		return printJSON(os.Stdout, infoResult{Exists: true, Info: info})
	}
	return failJSON("kill: session %q not found", id)
}

// sendAction joins the trailing arguments with single spaces and writes exactly
// those raw bytes to the session PTY, with no trailing newline; callers send any
// terminators (e.g. a carriage return) themselves.
func sendAction(_ context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) == 0 {
		return failJSON("send: an id is required")
	}
	id, text := args[0], args[1:]
	if id == "" {
		return failJSON("send: id must not be empty")
	}
	resp, err := dialRequest(id, daemon.Request{Op: "send", Input: []byte(strings.Join(text, " "))})
	if err != nil {
		return failJSON("send: session %q not found", id)
	}
	if !resp.OK {
		return failJSON("send: %s", resp.Error)
	}
	return printJSON(os.Stdout, map[string]any{"id": id, "sent": true})
}

// historyAction writes the raw head+tail scrollback bytes to stdout, querying a
// live daemon first and falling back to the persisted history of an ended
// session. Its output is intentionally not JSON.
func historyAction(_ context.Context, cmd *cli.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON("history: an id is required")
	}
	if resp, err := dialRequest(id, daemon.Request{Op: "history"}); err == nil && resp.OK {
		_, werr := os.Stdout.Write(resp.History)
		return werr
	}
	if data, err := os.ReadFile(daemon.HistoryPath(retentionDir(), id)); err == nil {
		_, werr := os.Stdout.Write(data)
		return werr
	}
	return failJSON("history: session %q not found", id)
}
