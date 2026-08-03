//go:build windows

package bgx

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestNormalizeTerminalSizeError(t *testing.T) {
	for _, expected := range []error{
		windows.ERROR_INVALID_HANDLE,
		windows.ERROR_INVALID_FUNCTION,
	} {
		err := normalizeTerminalSizeError(expected)
		if !errors.Is(err, ErrTerminalSizeUnavailable) {
			t.Errorf("normalizeTerminalSizeError(%v) = %v, want unavailable", expected, err)
		}
		if !errors.Is(err, expected) {
			t.Errorf("normalizeTerminalSizeError(%v) = %v, want original identity", expected, err)
		}
	}

	unexpected := windows.ERROR_ACCESS_DENIED
	if err := normalizeTerminalSizeError(unexpected); err != unexpected {
		t.Fatalf("normalizeTerminalSizeError(unexpected) = %v, want original error", err)
	}
}
