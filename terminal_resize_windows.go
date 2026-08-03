//go:build windows

package bgx

import "context"

// ResizeEvents closes when ctx ends because Windows has no SIGWINCH equivalent.
func (t *ProcessTerminal) ResizeEvents(ctx context.Context) <-chan struct{} {
	events := make(chan struct{})
	go func() {
		defer close(events)
		<-ctx.Done()
	}()
	return events
}
