//go:build !windows

package bgx

import (
	"errors"
	"syscall"
)

func normalizeTerminalSizeError(err error) error {
	if errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.EINVAL) {
		return errors.Join(ErrTerminalSizeUnavailable, err)
	}
	return err
}
