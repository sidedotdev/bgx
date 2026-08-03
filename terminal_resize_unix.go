//go:build !windows

package bgx

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// ResizeEvents reports process terminal resize notifications until ctx ends.
func (t *ProcessTerminal) ResizeEvents(ctx context.Context) <-chan struct{} {
	events := make(chan struct{}, 1)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	go func() {
		defer close(events)
		defer signal.Stop(signals)
		for {
			select {
			case <-ctx.Done():
				return
			case <-signals:
				select {
				case events <- struct{}{}:
				default:
				}
			}
		}
	}()
	return events
}
