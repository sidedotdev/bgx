//go:build !windows

package bgx

import (
	"context"
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

// ReadContext reads terminal input while allowing attach shutdown to interrupt
// an otherwise idle terminal.
func (t *ProcessTerminal) ReadContext(ctx context.Context, p []byte) (int, error) {
	fd := int(t.in.Fd())
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		ready, err := unix.Poll([]unix.PollFd{{
			Fd:     int32(fd),
			Events: unix.POLLIN,
		}}, 100)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return 0, err
		}
		if ready == 0 {
			continue
		}
		return t.in.Read(p)
	}
}
