//go:build !windows

package bgx

import (
	"errors"
	"syscall"
	"testing"
)

func TestNormalizeTerminalSizeError(t *testing.T) {
	for _, expected := range []error{syscall.ENOTTY, syscall.EINVAL} {
		err := normalizeTerminalSizeError(expected)
		if !errors.Is(err, ErrTerminalSizeUnavailable) {
			t.Errorf("normalizeTerminalSizeError(%v) = %v, want unavailable", expected, err)
		}
		if !errors.Is(err, expected) {
			t.Errorf("normalizeTerminalSizeError(%v) = %v, want original identity", expected, err)
		}
	}

	unexpected := errors.New("terminal size failed")
	if err := normalizeTerminalSizeError(unexpected); err != unexpected {
		t.Fatalf("normalizeTerminalSizeError(unexpected) = %v, want original error", err)
	}
}
