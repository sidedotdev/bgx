package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sidedotdev/bgx"
	urfave "github.com/urfave/cli/v3"
)

const (
	codeInvalidArgument  = "invalid_argument"
	codeNotFound         = "not_found"
	codeAlreadyExists    = "already_exists"
	codeConcurrencyLimit = "concurrency_limit"
	codeStartupFailed    = "startup_failed"
	codeFilesystem       = "filesystem"
	codeInternal         = "internal"
	codeSessionNotFound  = "session_not_found"
	codeSessionEnded     = "session_ended"
	codeAttachFailed     = "attach_failed"
	codeBridgeFailed     = "bridge_failed"

	defaultConcurrency = 3
	maxSocketPathLen   = 104
	errorSource        = "bgx"
)

// ExitError carries the process status requested by a successful command such
// as wait.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("exit status %d", e.Code)
}

type codedError struct {
	code    string
	err     error
	payload map[string]any
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

type operations struct {
	ensureDirs func() error
	run        func(string, []string, bgx.RunSpec) (*bgx.SessionInfo, error)
	info       func(context.Context, string) (*bgx.InfoResult, error)
	wait       func(context.Context, string) (*bgx.ExitResult, error)
	kill       func(context.Context, string) (*bgx.InfoResult, error)
	send       func(context.Context, string, []byte) (*bgx.SendResult, error)
	history    func(context.Context, string) ([]byte, error)
	attach     func(context.Context, string, bgx.AttachOptions) error
	bridge     func(context.Context, string) error
	list       func(bgx.ListOptions) []*bgx.SessionInfo
	version    func() bgx.VersionInfo
}

func defaultOperations() operations {
	return operations{
		ensureDirs: bgx.EnsureDirs,
		run:        bgx.Run,
		info:       bgx.Info,
		wait:       bgx.Wait,
		kill:       bgx.Kill,
		send:       bgx.Send,
		history:    bgx.History,
		attach:     bgx.Attach,
		bridge: func(ctx context.Context, id string) error {
			return bgx.Bridge(ctx, id, newStdioStream())
		},
		list:    bgx.List,
		version: bgx.Version,
	}
}

type runner struct {
	stdout io.Writer
	stderr io.Writer
	ops    operations
}

// Run executes the internal bgx command adapter. Successful wait commands may
// return an ExitError so the executable can mirror the session's exit status.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return newRunner(stdout, stderr, defaultOperations()).run(ctx, args)
}

func newRunner(stdout, stderr io.Writer, ops operations) *runner {
	return &runner{stdout: stdout, stderr: stderr, ops: ops}
}

type commandErrorKey struct{}

func (r *runner) run(ctx context.Context, args []string) error {
	var commandErr error
	ctx = context.WithValue(ctx, commandErrorKey{}, &commandErr)
	if err := r.rootCommand().Run(ctx, args); err != nil {
		commandErr = err
	}
	if commandErr == nil {
		return nil
	}

	var exit *ExitError
	if errors.As(commandErr, &exit) {
		return exit
	}

	var ce *codedError
	var payload map[string]any
	code := codeInternal
	if errors.As(commandErr, &ce) {
		code = ce.code
		payload = ce.payload
	}
	return errors.Join(commandErr, emitErrorJSON(r.stderr, code, commandErr.Error(), payload))
}

func (r *runner) rootCommand() *urfave.Command {
	root := &urfave.Command{
		Name:        "bgx",
		Usage:       "manage async terminal sessions",
		HideVersion: true,
		Commands: []*urfave.Command{
			r.runCommand(),
			r.waitCommand(),
			r.killCommand(),
			r.historyCommand(),
			r.attachCommand(),
			r.bridgeCommand(),
			r.sendCommand(),
			r.infoCommand(),
			r.listCommand(),
			r.versionCommand(),
		},
	}
	for _, command := range root.Commands {
		command.Action = r.withDirs(command.Action)
	}
	applyJSONUsageErrors(root)
	return root
}

func (r *runner) runCommand() *urfave.Command {
	stopAfterID := 1
	return &urfave.Command{
		Name:         "run",
		Usage:        "run a command async in a new session",
		ArgsUsage:    "[--overwrite-id] [--metadata key=value...] <id> <command...>",
		StopOnNthArg: &stopAfterID,
		Flags: []urfave.Flag{
			&urfave.BoolFlag{Name: "overwrite-id"},
			&urfave.StringSliceFlag{Name: "metadata"},
			&urfave.IntFlag{Name: "head-size"},
			&urfave.IntFlag{Name: "tail-size"},
			&urfave.StringFlag{Name: "storage"},
			&urfave.StringFlag{Name: "storage-path"},
			&urfave.IntFlag{Name: "retention", Sources: urfave.EnvVars("BGX_RETENTION")},
			&urfave.IntFlag{Name: "concurrency", Value: defaultConcurrency, Sources: urfave.EnvVars("BGX_CONCURRENCY")},
		},
		Action: r.runAction,
	}
}

func (r *runner) waitCommand() *urfave.Command {
	return &urfave.Command{
		Name:      "wait",
		Usage:     "wait for a session to finish and return its exit code",
		ArgsUsage: "<id>",
		Action:    r.waitAction,
	}
}

func (r *runner) killCommand() *urfave.Command {
	return &urfave.Command{
		Name:      "kill",
		Usage:     "kill a running session",
		ArgsUsage: "<id>",
		Action:    r.killAction,
	}
}

func (r *runner) historyCommand() *urfave.Command {
	return &urfave.Command{
		Name:      "history",
		Usage:     "print the scrollback history of a session",
		ArgsUsage: "<id>",
		Action:    r.historyAction,
	}
}

func (r *runner) attachCommand() *urfave.Command {
	stopAfterID := 1
	return &urfave.Command{
		Name:         "attach",
		Usage:        "attach to a running session",
		ArgsUsage:    "[--ssh <host>] <id> [--show-detach-instructions] [--via <cmd...>]",
		StopOnNthArg: &stopAfterID,
		Flags: []urfave.Flag{
			&urfave.BoolFlag{Name: "show-detach-instructions"},
			&urfave.StringFlag{Name: "ssh"},
		},
		Action: r.attachAction,
	}
}

func (r *runner) bridgeCommand() *urfave.Command {
	return &urfave.Command{
		Name:      "bridge",
		Usage:     "forward one raw session connection over stdio",
		ArgsUsage: "<id>",
		Action:    r.bridgeAction,
	}
}

func (r *runner) sendCommand() *urfave.Command {
	return &urfave.Command{
		Name:      "send",
		Usage:     "send raw input to a session PTY without attaching",
		ArgsUsage: "<id> <text...>",
		Action:    r.sendAction,
	}
}

func (r *runner) infoCommand() *urfave.Command {
	return &urfave.Command{
		Name:      "info",
		Usage:     "print metadata about a session",
		ArgsUsage: "<id>",
		Action:    r.infoAction,
	}
}

func (r *runner) listCommand() *urfave.Command {
	return &urfave.Command{
		Name:    "list",
		Aliases: []string{"ls"},
		Usage:   "list sessions",
		Flags: []urfave.Flag{
			&urfave.StringSliceFlag{Name: "metadata"},
		},
		Action: r.listAction,
	}
}

func (r *runner) versionCommand() *urfave.Command {
	return &urfave.Command{
		Name:   "version",
		Usage:  "print version and environment info",
		Action: r.versionAction,
	}
}

func (r *runner) runAction(_ context.Context, cmd *urfave.Command) error {
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

	version := r.ops.version()
	if len(filepath.Join(version.SocketDir, url.QueryEscape(id)+".sock")) > maxSocketPathLen {
		return failJSON(codeInvalidArgument, "run: socket path for id %q exceeds %d bytes", id, maxSocketPathLen)
	}

	info, err := r.ops.run(id, command, bgx.RunSpec{
		OverwriteID: cmd.Bool("overwrite-id"),
		Metadata:    metadata,
		HeadSize:    cmd.Int("head-size"),
		TailSize:    cmd.Int("tail-size"),
		Storage:     cmd.String("storage"),
		StoragePath: cmd.String("storage-path"),
		Retention:   cmd.Int("retention"),
		Concurrency: cmd.Int("concurrency"),
	})
	if err != nil {
		return runError(id, err)
	}

	result := map[string]any{
		"id":         info.ID,
		"pid":        info.Pid,
		"started_at": info.StartedAt,
	}
	if version.Fallback != "" {
		result["fallback"] = version.Fallback
		result["socket_dir"] = version.SocketDir
		result["retention_dir"] = version.RetentionDir
	}
	return printJSON(r.stdout, result)
}

func runError(id string, err error) error {
	var limit *bgx.ConcurrencyLimitError
	var startup *bgx.StartupError
	switch {
	case errors.Is(err, bgx.ErrSessionRunning):
		return failJSON(codeAlreadyExists, "run: session %q is already running", id)
	case errors.Is(err, bgx.ErrSessionExists):
		return failJSON(codeAlreadyExists, "run: session %q already exists; pass --overwrite-id to replace it", id)
	case errors.As(err, &limit):
		label := fmt.Sprintf("namespace %q", limit.Namespace)
		if limit.Namespace == "" {
			label = "the global namespace"
		}
		return &codedError{
			code: codeConcurrencyLimit,
			err: fmt.Errorf(
				"run: %s already has %d active session(s); concurrency limit is %d",
				label,
				len(limit.Active),
				limit.Limit,
			),
			payload: map[string]any{"sessions": limit.Active},
		}
	case errors.As(err, &startup):
		return failJSON(codeStartupFailed, "run: %v", err)
	default:
		return failJSON(codeInternal, "run: %v", err)
	}
}

func (r *runner) infoAction(ctx context.Context, cmd *urfave.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON(codeInvalidArgument, "info: an id is required")
	}
	result, err := r.ops.info(ctx, id)
	if err != nil {
		return failJSON(codeInternal, "info: %v", err)
	}
	if !result.Exists {
		return printJSON(r.stdout, map[string]any{"id": id, "exists": false})
	}
	return printJSON(r.stdout, result)
}

func (r *runner) waitAction(ctx context.Context, cmd *urfave.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON(codeInvalidArgument, "wait: an id is required")
	}
	result, err := r.ops.wait(ctx, id)
	if err != nil {
		var missing *bgx.SessionNotFoundError
		if errors.As(err, &missing) {
			return failJSON(codeNotFound, "wait: session %q not found", id)
		}
		return failJSON(codeInternal, "wait: %v", err)
	}
	if err := printJSON(r.stdout, map[string]any{"id": id, "exit_code": result.ExitCode}); err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return &ExitError{Code: result.ExitCode}
	}
	return nil
}

func (r *runner) killAction(ctx context.Context, cmd *urfave.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON(codeInvalidArgument, "kill: an id is required")
	}
	result, err := r.ops.kill(ctx, id)
	if err != nil {
		var missing *bgx.SessionNotFoundError
		if errors.As(err, &missing) {
			return failJSON(codeNotFound, "kill: session %q not found", id)
		}
		return failJSON(codeInternal, "kill: %v", err)
	}
	return printJSON(r.stdout, result)
}

func (r *runner) attachAction(ctx context.Context, cmd *urfave.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON(codeInvalidArgument, "attach: an id is required")
	}
	opts := bgx.AttachOptions{
		ShowDetachInstructions: cmd.Bool("show-detach-instructions"),
		SSH:                    cmd.String("ssh"),
	}
	rest := cmd.Args().Slice()[1:]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--show-detach-instructions":
			opts.ShowDetachInstructions = true
		case "--ssh":
			if i == len(rest)-1 {
				return failJSON(codeInvalidArgument, "attach: --ssh requires a host")
			}
			i++
			opts.SSH = rest[i]
		case "--via":
			if i == len(rest)-1 {
				return failJSON(codeInvalidArgument, "attach: --via requires a command")
			}
			opts.Via = rest[i+1:]
			i = len(rest)
		default:
			return failJSON(codeInvalidArgument, "attach: unexpected argument %q", rest[i])
		}
	}

	err := r.ops.attach(ctx, id, opts)
	if err == nil {
		return nil
	}
	var optionsErr *bgx.AttachOptionsError
	if errors.As(err, &optionsErr) {
		return failJSON(codeInvalidArgument, "%v", optionsErr)
	}
	var notFound *bgx.SessionNotFoundError
	if errors.As(err, &notFound) {
		return failJSON(codeSessionNotFound, "attach: session %q does not exist", id)
	}
	var ended *bgx.SessionEndedError
	if errors.As(err, &ended) {
		return failJSON(codeSessionEnded, "attach: session %q has already ended", id)
	}
	var responseErr *bgx.ResponseError
	if errors.As(err, &responseErr) {
		return failJSON(codeAttachFailed, "attach: %s", responseErr.Message)
	}
	return failJSON(codeAttachFailed, "attach: %v", err)
}

func (r *runner) bridgeAction(ctx context.Context, cmd *urfave.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON(codeInvalidArgument, "bridge: an id is required")
	}
	info, err := r.ops.info(ctx, id)
	if err != nil {
		return failJSON(codeBridgeFailed, "bridge: %v", err)
	}
	if !info.Exists {
		return failJSON(codeSessionNotFound, "bridge: session %q does not exist", id)
	}
	if info.SessionInfo == nil || !info.Running {
		return failJSON(codeSessionEnded, "bridge: session %q has already ended", id)
	}
	if err := r.ops.bridge(ctx, id); err != nil {
		info, infoErr := r.ops.info(ctx, id)
		if infoErr == nil {
			if !info.Exists {
				return failJSON(codeSessionNotFound, "bridge: session %q does not exist", id)
			}
			if info.SessionInfo == nil || !info.Running {
				return failJSON(codeSessionEnded, "bridge: session %q has already ended", id)
			}
		}
		return failJSON(codeBridgeFailed, "%v", err)
	}
	return nil
}

func (r *runner) sendAction(ctx context.Context, cmd *urfave.Command) error {
	args := cmd.Args().Slice()
	if len(args) == 0 {
		return failJSON(codeInvalidArgument, "send: an id is required")
	}
	id, text := args[0], args[1:]
	if id == "" {
		return failJSON(codeInvalidArgument, "send: id must not be empty")
	}
	result, err := r.ops.send(ctx, id, []byte(strings.Join(text, " ")))
	if err != nil {
		var missing *bgx.SessionNotFoundError
		var response *bgx.ResponseError
		switch {
		case errors.As(err, &missing):
			return failJSON(codeNotFound, "send: session %q not found", id)
		case errors.As(err, &response):
			return failJSON(codeInternal, "send: %s", response.Message)
		default:
			return failJSON(codeInternal, "send: %v", err)
		}
	}
	return printJSON(r.stdout, result)
}

func (r *runner) historyAction(ctx context.Context, cmd *urfave.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON(codeInvalidArgument, "history: an id is required")
	}
	data, err := r.ops.history(ctx, id)
	if err != nil {
		var missing *bgx.SessionNotFoundError
		if errors.As(err, &missing) {
			return failJSON(codeNotFound, "history: session %q not found", id)
		}
		return failJSON(codeInternal, "history: %v", err)
	}
	_, err = r.stdout.Write(data)
	return err
}

func (r *runner) listAction(_ context.Context, cmd *urfave.Command) error {
	metadata, err := parseMetadata(cmd.StringSlice("metadata"))
	if err != nil {
		return failJSON(codeInvalidArgument, "list: %v", err)
	}
	return printJSON(r.stdout, r.ops.list(bgx.ListOptions{Metadata: metadata}))
}

func (r *runner) versionAction(_ context.Context, _ *urfave.Command) error {
	return printJSON(r.stdout, r.ops.version())
}

func parseMetadata(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	metadata := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid metadata %q, expected key=value", entry)
		}
		metadata[key] = value
	}
	return metadata, nil
}

func printJSON(w io.Writer, value any) error {
	return json.NewEncoder(w).Encode(value)
}

func emitErrorJSON(w io.Writer, code, message string, extra map[string]any) error {
	payload := map[string]any{
		"error":  message,
		"code":   code,
		"source": errorSource,
	}
	for key, value := range extra {
		if key != "error" && key != "code" && key != "source" {
			payload[key] = value
		}
	}
	return printJSON(w, payload)
}

func failJSON(code, format string, args ...any) error {
	return &codedError{code: code, err: fmt.Errorf(format, args...)}
}

func applyJSONUsageErrors(cmd *urfave.Command) {
	cmd.OnUsageError = func(_ context.Context, _ *urfave.Command, err error, _ bool) error {
		return &codedError{code: codeInvalidArgument, err: err}
	}
	cmd.CommandNotFound = func(ctx context.Context, _ *urfave.Command, name string) {
		if commandErr, ok := ctx.Value(commandErrorKey{}).(*error); ok {
			*commandErr = failJSON(codeInvalidArgument, "unknown command %q", name)
		}
	}
	for _, subcommand := range cmd.Commands {
		applyJSONUsageErrors(subcommand)
	}
}
func (r *runner) withDirs(action urfave.ActionFunc) urfave.ActionFunc {
	return func(ctx context.Context, cmd *urfave.Command) error {
		if err := r.ops.ensureDirs(); err != nil {
			return failJSON(codeFilesystem, "%v", err)
		}
		return action(ctx, cmd)
	}
}

type stdioStream struct {
	closeOnce sync.Once
	closeErr  error
}

func newStdioStream() *stdioStream {
	return &stdioStream{}
}

func (s *stdioStream) Read(p []byte) (int, error) {
	return os.Stdin.Read(p)
}

func (s *stdioStream) Write(p []byte) (int, error) {
	return os.Stdout.Write(p)
}

func (s *stdioStream) Close() error {
	s.closeOnce.Do(func() {
		err := os.Stdin.Close()
		if closeErr := os.Stdout.Close(); err == nil {
			err = closeErr
		}
		s.closeErr = err
	})
	return s.closeErr
}
