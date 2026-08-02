package bgx

// The socket-path scheme and stale-socket cleanup here are ported from zmx
// (https://github.com/neurosnap/zmx); see LICENSE-zmx for its license.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sidedotdev/bgx/daemon"
	"github.com/sidedotdev/bgx/scrollback"
	cli "github.com/urfave/cli/v3"
)

// maxSocketPathLen is a conservative cap on a unix domain socket path length.
// Linux allows 108 and macOS 104 bytes for sun_path; the smaller bound keeps a
// given id portable across both.
const maxSocketPathLen = 104

// socketReadyTimeout bounds how long run waits for a freshly spawned daemon to
// bind its socket (or, for a very short-lived command, to persist a record).
const socketReadyTimeout = 5 * time.Second

// defaultConcurrency caps how many sessions may be active at once within a
// single id namespace unless overridden via the run --concurrency flag.
const defaultConcurrency = 3

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

// sessionClient builds a typed client whose operations each dial one fresh
// connection to the local session's socket.
func sessionClient(id string) *Client {
	return NewClient(func(ctx context.Context) (io.ReadWriteCloser, error) {
		return Dial(ctx, id)
	})
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

func runAction(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) == 0 {
		return failJSON(codeInvalidArgument, "run: an id is required")
	}
	id, command := args[0], args[1:]
	if id == "" {
		return failJSON(codeInvalidArgument, "run: id must not be empty")
	}
	if len(command) == 0 {
		return failJSON(codeInvalidArgument, "run: a command is required")
	}
	metadata, err := parseMetadata(cmd.StringSlice("metadata"))
	if err != nil {
		return failJSON(codeInvalidArgument, "run: %v", err)
	}
	if len(socketPath(id)) > maxSocketPathLen {
		return failJSON(codeInvalidArgument, "run: socket path for id %q exceeds %d bytes", id, maxSocketPathLen)
	}

	info, err := Start(ctx, id, command, StartOptions{
		Metadata:       metadata,
		OverwriteID:    cmd.Bool("overwrite-id"),
		Concurrency:    cmd.Int("concurrency"),
		RetentionCount: cmd.Int("retention"),
		Scrollback: scrollback.Config{
			HeadSize:    cmd.Int("head-size"),
			TailSize:    cmd.Int("tail-size"),
			Storage:     scrollback.StorageKind(cmd.String("storage")),
			StoragePath: cmd.String("storage-path"),
		},
	})
	if err != nil {
		var climit *ConcurrencyLimitError
		var startup *StartupError
		switch {
		case errors.Is(err, ErrSessionRunning):
			return failJSON(codeAlreadyExists, "run: session %q is already running", id)
		case errors.Is(err, ErrSessionExists):
			return failJSON(codeAlreadyExists, "run: session %q already exists; pass --overwrite-id to replace it", id)
		case errors.As(err, &climit):
			return failConcurrencyLimit(climit.Namespace, climit.Limit, climit.Active)
		case errors.As(err, &startup):
			return failJSON(codeStartupFailed, "run: %v", err)
		default:
			return failJSON(codeInternal, "run: %v", err)
		}
	}
	result := map[string]any{
		"id":         info.ID,
		"pid":        info.Pid,
		"started_at": info.StartedAt,
	}
	if notice := fallbackNotice(); notice != "" {
		result["fallback"] = notice
		result["socket_dir"] = socketDir()
		result["retention_dir"] = retentionDir()
	}
	return printJSON(os.Stdout, result)
}

// spawnDaemon re-execs the current executable with the private daemon-config
// marker in its environment so InterceptDaemon runs the session daemon, in its
// own session with detached stdio so the session outlives this client. It
// returns the started process so the caller can detect an early exit during
// startup, plus the path to a temporary file capturing the daemon's stderr so
// a startup failure can surface the daemon's own explanation rather than only
// its exit status.
func spawnDaemon(cfg daemon.Config) (*exec.Cmd, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, "", err
	}
	payload, err := json.Marshal(cfg)
	if err != nil {
		return nil, "", err
	}

	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, "", err
	}
	defer devnull.Close()

	stderr, err := os.CreateTemp("", "bgx-daemon-stderr-*")
	if err != nil {
		return nil, "", err
	}
	defer stderr.Close()

	dc := exec.Command(exe)
	dc.Env = append(os.Environ(), daemonConfigEnv+"="+string(payload))
	dc.Stdin = devnull
	dc.Stdout = devnull
	dc.Stderr = stderr
	dc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := dc.Start(); err != nil {
		os.Remove(stderr.Name())
		return nil, "", err
	}
	return dc, stderr.Name(), nil
}

// waitForSession blocks until a freshly spawned session answers on its socket
// or, for a command that already exited, has persisted a record, returning the
// resulting metadata snapshot. A daemon must outlive its client, so if the
// spawned process exits before either happens, the startup failure is surfaced
// promptly instead of waiting out the readiness timeout.
func waitForSession(ctx context.Context, id string, proc *exec.Cmd, stderrPath string, timeout time.Duration) (*daemon.Info, error) {
	exited := make(chan error, 1)
	go func() { exited <- proc.Wait() }()

	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if info, ok := liveInfo(id); ok {
			return info, nil
		}
		if info, ok := endedRecord(id); ok {
			return info, nil
		}
		select {
		case werr := <-exited:
			// The daemon exited during startup. Re-check for a live socket, then
			// give a very short-lived but valid session's ended record a brief
			// bounded window to become observable, since Wait() can win the race
			// against the record's fsync/rename. Only then report the exit as an
			// actionable startup error.
			if info, ok := liveInfo(id); ok {
				return info, nil
			}
			if info, ok := recheckEndedRecord(id, 250*time.Millisecond); ok {
				return info, nil
			}
			return nil, startupError(id, werr, stderrPath)
		default:
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for session %q to start", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// recheckEndedRecord polls for an ended record over a short bounded window,
// closing the race where the daemon process has exited but its persisted record
// has not yet become observable to the client.
func recheckEndedRecord(id string, within time.Duration) (*daemon.Info, bool) {
	deadline := time.Now().Add(within)
	for {
		if info, ok := endedRecord(id); ok {
			return info, true
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startupError builds the run failure for a daemon that exited before its
// session became available, preferring the daemon's own stderr so the message
// is actionable and falling back to the process exit status otherwise.
func startupError(id string, werr error, stderrPath string) error {
	if line := firstStderrLine(stderrPath); line != "" {
		return fmt.Errorf("session %q daemon exited before startup completed: %s", id, line)
	}
	if werr != nil {
		return fmt.Errorf("session %q daemon exited before startup completed: %v", id, werr)
	}
	return fmt.Errorf("session %q daemon exited before startup completed", id)
}

// firstStderrLine returns the first non-blank line captured from the daemon's
// stderr file, or the empty string if none is available.
func firstStderrLine(path string) string {
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func infoAction(ctx context.Context, cmd *cli.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON(codeInvalidArgument, "info: an id is required")
	}
	if info, err := sessionClient(id).Info(ctx); err == nil {
		return printJSON(os.Stdout, infoResult{Exists: true, Info: info})
	}
	if info, ok := EndedRecord(id); ok {
		return printJSON(os.Stdout, infoResult{Exists: true, Info: info})
	}
	return printJSON(os.Stdout, map[string]any{"id": id, "exists": false})
}

func waitAction(ctx context.Context, cmd *cli.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON(codeInvalidArgument, "wait: an id is required")
	}
	if res, err := sessionClient(id).Wait(ctx); err == nil {
		return emitExit(id, res.ExitCode)
	}
	if info, ok := EndedRecord(id); ok && info.ExitCode != nil {
		return emitExit(id, *info.ExitCode)
	}
	// A daemon shutting down closes its listener before persisting its record,
	// so a dial can fail while the record is still in flight. A lingering socket
	// file marks that teardown; give the record a bounded window to appear
	// before concluding the session is unknown.
	if _, err := os.Stat(socketPath(id)); err == nil {
		if info, ok := recheckEndedRecord(id, 2*time.Second); ok && info.ExitCode != nil {
			return emitExit(id, *info.ExitCode)
		}
	}
	return failJSON(codeNotFound, "wait: session %q not found", id)
}

// emitExit prints the session's exit code as JSON and mirrors it as the bgx
// process exit status.
func emitExit(id string, code int) error {
	_ = printJSON(os.Stdout, map[string]any{"id": id, "exit_code": code})
	os.Exit(code)
	return nil
}

func killAction(ctx context.Context, cmd *cli.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON(codeInvalidArgument, "kill: an id is required")
	}
	if res, err := sessionClient(id).Kill(ctx); err == nil {
		return printJSON(os.Stdout, infoResult{Exists: true, Info: res.Info})
	}
	if info, ok := EndedRecord(id); ok {
		return printJSON(os.Stdout, infoResult{Exists: true, Info: info})
	}
	return failJSON(codeNotFound, "kill: session %q not found", id)
}

// sendAction joins the trailing arguments with single spaces and writes exactly
// those raw bytes to the session PTY, with no trailing newline; callers send any
// terminators (e.g. a carriage return) themselves.
func sendAction(ctx context.Context, cmd *cli.Command) error {
	args := cmd.Args().Slice()
	if len(args) == 0 {
		return failJSON(codeInvalidArgument, "send: an id is required")
	}
	id, text := args[0], args[1:]
	if id == "" {
		return failJSON(codeInvalidArgument, "send: id must not be empty")
	}
	err := sessionClient(id).Send(ctx, []byte(strings.Join(text, " ")))
	var respErr *ResponseError
	switch {
	case err == nil:
		return printJSON(os.Stdout, map[string]any{"id": id, "sent": true})
	case errors.As(err, &respErr):
		return failJSON(codeInternal, "send: %s", respErr.Message)
	default:
		return failJSON(codeNotFound, "send: session %q not found", id)
	}
}

// historyAction writes the raw head+tail scrollback bytes to stdout, querying a
// live daemon first and falling back to the persisted history of an ended
// session. Its output is intentionally not JSON.
func historyAction(ctx context.Context, cmd *cli.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON(codeInvalidArgument, "history: an id is required")
	}
	if data, err := sessionClient(id).History(ctx); err == nil {
		_, werr := os.Stdout.Write(data)
		return werr
	}
	if data, err := os.ReadFile(daemon.HistoryPath(retentionDir(), id)); err == nil {
		_, werr := os.Stdout.Write(data)
		return werr
	}
	return failJSON(codeNotFound, "history: session %q not found", id)
}

func listAction(_ context.Context, cmd *cli.Command) error {
	filters, err := parseMetadata(cmd.StringSlice("metadata"))
	if err != nil {
		return failJSON(codeInvalidArgument, "list: %v", err)
	}
	return printJSON(os.Stdout, ListSessions(ListOptions{Metadata: filters}))
}

// lockNamespace takes an exclusive advisory lock that serializes run's
// concurrency check and daemon spawn within a single id namespace, so
// concurrent run invocations cannot race past the configured cap. The returned
// release function must be called once the new session is observable. The lock
// is also released automatically if the process exits while holding it.
func lockNamespace(ns string) (func(), error) {
	dir := socketDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, url.QueryEscape(ns)+".nslock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// runningInNamespace returns the live sessions whose ids share the given
// namespace (the portion before the first "/"), used to enforce the per-
// namespace concurrency limit.
func runningInNamespace(ns string) []*daemon.Info {
	var out []*daemon.Info
	for _, info := range listRunning() {
		if daemon.Namespace(info.ID) == ns {
			out = append(out, info)
		}
	}
	return out
}

// failConcurrencyLimit reports that a namespace is already at its active-session
// limit, including every offending session so callers can act on the listing.
func failConcurrencyLimit(ns string, limit int, sessions []*daemon.Info) error {
	label := fmt.Sprintf("namespace %q", ns)
	if ns == "" {
		label = "the global namespace"
	}
	_ = printJSON(os.Stderr, map[string]any{
		"error": fmt.Sprintf("run: %s already has %d active session(s); concurrency limit is %d",
			label, len(sessions), limit),
		"code":     codeConcurrencyLimit,
		"source":   errorSource,
		"sessions": sessions,
	})
	os.Exit(1)
	return nil
}

// listRunning queries every live session socket, cleaning up sockets that no
// daemon answers on so a crashed session doesn't linger in listings.
func listRunning() []*daemon.Info {
	dir := socketDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []*daemon.Info
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sock") {
			continue
		}
		id, err := url.QueryUnescape(strings.TrimSuffix(name, ".sock"))
		if err != nil {
			continue
		}
		if info, ok := liveInfo(id); ok {
			out = append(out, info)
			continue
		}
		os.Remove(filepath.Join(dir, name))
	}
	return out
}

// listEnded reads every persisted ended-session record across all namespaces.
func listEnded() []*daemon.Info {
	base := retentionDir()
	namespaces, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []*daemon.Info
	for _, ns := range namespaces {
		if !ns.IsDir() {
			continue
		}
		nsPath := filepath.Join(base, ns.Name())
		records, err := os.ReadDir(nsPath)
		if err != nil {
			continue
		}
		for _, r := range records {
			if r.IsDir() || !strings.HasSuffix(r.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(nsPath, r.Name()))
			if err != nil {
				continue
			}
			var info daemon.Info
			if json.Unmarshal(data, &info) != nil {
				continue
			}
			info.Running = false
			out = append(out, &info)
		}
	}
	return out
}

// matchesMetadata reports whether info's metadata satisfies every filter.
func matchesMetadata(info *daemon.Info, filters map[string]string) bool {
	for k, v := range filters {
		if info.Metadata[k] != v {
			return false
		}
	}
	return true
}
