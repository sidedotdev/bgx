package bgx

import (
	"context"
	"errors"
	"fmt"
	"os"

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
	code string
	err  error
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

// emitErrorJSON writes the machine-readable error payload to stderr, keeping
// stdout reserved for successful command output.
func emitErrorJSON(code, msg string) {
	_ = printJSON(os.Stderr, map[string]string{"error": msg, "code": code, "source": errorSource})
}

// failJSON prints a JSON error object (message plus code) to stderr and exits
// non-zero so failures stay machine-readable for every command.
func failJSON(code, format string, args ...any) error {
	emitErrorJSON(code, fmt.Sprintf(format, args...))
	os.Exit(1)
	return nil
}

// applyJSONUsageErrors suppresses urfave/cli's plain-text usage-error and
// unknown-command output on cmd and all its subcommands, so parse failures
// surface as JSON on stderr like every other error.
func applyJSONUsageErrors(cmd *cli.Command) {
	cmd.OnUsageError = func(_ context.Context, _ *cli.Command, err error, _ bool) error {
		return &codedError{code: codeInvalidArgument, err: err}
	}
	cmd.CommandNotFound = func(_ context.Context, _ *cli.Command, name string) {
		_ = failJSON(codeInvalidArgument, "unknown command %q", name)
	}
	for _, sub := range cmd.Commands {
		applyJSONUsageErrors(sub)
	}
}
