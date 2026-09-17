package bgx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/sidedotdev/bgx/daemon"
)

const attachEscapeTimeout = 25 * time.Millisecond

func runAttachInput(ctx context.Context, terminal Terminal, send func(daemon.FrameTag, []byte) error) (retErr error) {
	readCtx, cancelRead := context.WithCancel(ctx)
	type readResult struct {
		n   int
		err error
	}
	requests := make(chan struct{})
	results := make(chan readResult, 1)
	stopped := make(chan struct{})
	buf := make([]byte, 64<<10)
	go func() {
		defer close(stopped)
		for range requests {
			n, err := terminal.ReadContext(readCtx, buf)
			results <- readResult{n: n, err: err}
			if err != nil {
				return
			}
		}
	}()

	readPending := false
	defer func() {
		close(requests)
		cancelRead()
		<-stopped
		if readPending {
			read := <-results
			if err := withoutExpectedAttachShutdownErrors(read.err, true); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("attach: read terminal: %w", err))
			}
		}
	}()

	timer := time.NewTimer(attachEscapeTimeout)
	timer.Stop()
	defer timer.Stop()
	var scanner detachScanner
	flush := func() error {
		if pending := scanner.flush(); len(pending) > 0 {
			return send(daemon.FrameInput, pending)
		}
		return nil
	}
	for {
		// A new request transfers buffer ownership back to the reader. Keeping
		// that read alive across Escape expiry avoids canceling terminal input.
		requests <- struct{}{}
		readPending = true
		var timeout <-chan time.Time
		if len(scanner.pending) == 1 && scanner.pending[0] == 0x1b {
			timer.Reset(attachEscapeTimeout)
			timeout = timer.C
		}
		for readPending {
			select {
			case <-timeout:
				if err := flush(); err != nil {
					return err
				}
				timeout = nil
			case read := <-results:
				readPending = false
				timer.Stop()
				if read.n > 0 {
					forward, detach := scanner.feed(buf[:read.n])
					if len(forward) > 0 {
						if err := send(daemon.FrameInput, forward); err != nil {
							return err
						}
					}
					if detach {
						if err := send(daemon.FrameDetach, nil); err != nil {
							return err
						}
						if err := withoutExpectedAttachShutdownErrors(read.err, true); err != nil {
							return errors.Join(errAttachDetached, fmt.Errorf("attach: read terminal: %w", err))
						}
						return errAttachDetached
					}
				}
				if read.err != nil {
					if errors.Is(read.err, io.EOF) {
						if err := flush(); err != nil {
							return errors.Join(err, withoutExpectedAttachShutdownErrors(read.err, true))
						}
						read.err = withoutExpectedAttachShutdownErrors(read.err, true)
					}
					if read.err != nil {
						return fmt.Errorf("attach: read terminal: %w", read.err)
					}
					return nil
				}
			}
		}
	}
}
