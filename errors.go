package bgx

import (
	"context"
	"errors"
	"fmt"
	"io"

	cli "github.com/urfave/cli/v3"
)

// Error codes included alongside every JSON error message so callers can act
// on failures programmatically.
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
)

// codedError attaches a machine-readable code to an error so the top-level
// handler can include it in the JSON error payload.
type codedError struct {
	code    string
	err     error
	payload map[string]any
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

// errorCode extracts the code attached to err, defaulting to internal for
// errors that reach the top-level handler without one.
func errorCode(err error) string {
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code
	}
	return codeInternal
}

// errorSource marks JSON error payloads as originating from bgx itself, so
// consumers can distinguish them from output of the wrapped command.
const errorSource = "bgx"

// emitErrorJSON writes the machine-readable error payload to w, keeping stdout
// reserved for successful command output.
func emitErrorJSON(w io.Writer, code, msg string, extra ...map[string]any) error {
	payload := map[string]any{"error": msg, "code": code, "source": errorSource}
	for _, fields := range extra {
		for key, value := range fields {
			if key != "error" && key != "code" && key != "source" {
				payload[key] = value
			}
		}
	}
	return printJSON(w, payload)
}

// failJSON returns a coded error for the top-level runner to emit as JSON.
func failJSON(code, format string, args ...any) error {
	return &codedError{code: code, err: fmt.Errorf(format, args...)}
}

// applyJSONUsageErrors suppresses urfave/cli's plain-text usage-error and
// unknown-command output on cmd and all its subcommands, so parse failures
// surface as JSON on stderr like every other error.
type commandErrorKey struct{}

func applyJSONUsageErrors(cmd *cli.Command) {
	cmd.OnUsageError = func(_ context.Context, _ *cli.Command, err error, _ bool) error {
		return &codedError{code: codeInvalidArgument, err: err}
	}
	cmd.CommandNotFound = func(ctx context.Context, _ *cli.Command, name string) {
		if commandErr, ok := ctx.Value(commandErrorKey{}).(*error); ok {
			*commandErr = failJSON(codeInvalidArgument, "unknown command %q", name)
		}
	}
	for _, sub := range cmd.Commands {
		applyJSONUsageErrors(sub)
	}
}
