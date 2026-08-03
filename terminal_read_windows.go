//go:build windows

package bgx

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// ReadContext uses a duplicate handle so cancellation can interrupt this
// attachment without closing process stdin.
func (t *ProcessTerminal) ReadContext(ctx context.Context, p []byte) (int, error) {
	process := windows.CurrentProcess()
	var handle windows.Handle
	if err := windows.DuplicateHandle(
		process,
		windows.Handle(t.in.Fd()),
		process,
		&handle,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		return 0, err
	}

	input := os.NewFile(uintptr(handle), t.in.Name())
	result := make(chan struct {
		n   int
		err error
	}, 1)
	go func() {
		n, err := input.Read(p)
		result <- struct {
			n   int
			err error
		}{n: n, err: err}
	}()

	select {
	case read := <-result:
		return read.n, errors.Join(read.err, input.Close())
	case <-ctx.Done():
		cancelErr := windows.CancelIoEx(handle, nil)
		if errors.Is(cancelErr, windows.ERROR_NOT_FOUND) {
			cancelErr = nil
		}
		closeErr := input.Close()
		if errors.Is(closeErr, os.ErrClosed) {
			closeErr = nil
		}
		read := <-result
		readErr := withoutExpectedWindowsCancellationError(read.err)
		return read.n, errors.Join(readErr, cancelErr, closeErr, ctx.Err())
	}
}

type prunedWindowsTerminalReadError struct {
	message string
	err     error
}

func (e *prunedWindowsTerminalReadError) Error() string {
	return e.message
}

func (e *prunedWindowsTerminalReadError) Unwrap() error {
	return e.err
}

func withoutExpectedWindowsCancellationError(err error) error {
	if err == nil || err == windows.ERROR_OPERATION_ABORTED {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		remaining := make([]error, 0, len(children))
		for _, child := range children {
			if child = withoutExpectedWindowsCancellationError(child); child != nil {
				remaining = append(remaining, child)
			}
		}
		return errors.Join(remaining...)
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		child := withoutExpectedWindowsCancellationError(wrapped.Unwrap())
		if child == nil {
			return nil
		}
		return &prunedWindowsTerminalReadError{message: err.Error(), err: child}
	}
	return err
}
