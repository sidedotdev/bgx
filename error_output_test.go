package bgx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type failingJSONWriter struct {
	err error
}

func (w failingJSONWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func TestEmitErrorJSONReturnsWriteError(t *testing.T) {
	writeErr := errors.New("write stderr")
	err := emitErrorJSON(failingJSONWriter{err: writeErr}, codeInternal, "failed")
	if !errors.Is(err, writeErr) {
		t.Fatalf("emitErrorJSON error = %v, want write error", err)
	}
}

func TestRunCLIPropagatesErrorJSONWriteFailure(t *testing.T) {
	writeErr := errors.New("write stderr")
	err := runCLI(
		context.Background(),
		[]string{"bgx", "--definitely-not-a-flag"},
		failingJSONWriter{err: writeErr},
	)
	if !errors.Is(err, writeErr) {
		t.Fatalf("runCLI error = %v, want JSON write error", err)
	}
}

func TestRunCLIUnknownCommandReturnsCodedError(t *testing.T) {
	var stderr bytes.Buffer
	err := runCLI(context.Background(), []string{"bgx", "bogus-command"}, &stderr)
	if err == nil {
		t.Fatal("runCLI returned nil for unknown command")
	}
	if got := errorCode(err); got != codeInvalidArgument {
		t.Fatalf("error code = %q, want %q", got, codeInvalidArgument)
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("runCLI error = %q, want unknown-command message", err)
	}

	var payload map[string]string
	if decodeErr := json.Unmarshal(stderr.Bytes(), &payload); decodeErr != nil {
		t.Fatalf("decode stderr %q: %v", stderr.String(), decodeErr)
	}
	if got := payload["code"]; got != codeInvalidArgument {
		t.Fatalf("JSON error code = %q, want %q", got, codeInvalidArgument)
	}
}
