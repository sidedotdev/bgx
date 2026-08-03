//go:build windows

package bgx

import (
	"errors"

	"golang.org/x/sys/windows"
)

func normalizeTerminalSizeError(err error) error {
	if errors.Is(err, windows.ERROR_INVALID_HANDLE) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
		return errors.Join(ErrTerminalSizeUnavailable, err)
	}
	return err
}
