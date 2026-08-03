package bgx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
)

// Dial opens one connection to a local session's socket. Each connection
// carries exactly one operation of the socket protocol, so a closure over an
// id makes Dial the local Dialer for NewClient.
func Dial(ctx context.Context, id string) (io.ReadWriteCloser, error) {
	return dialSession(ctx, id)
}

// dialSession returns the concrete unix connection so Bridge can half-close
// its write side.
func dialSession(ctx context.Context, id string) (*net.UnixConn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath(id))
	if err != nil {
		return nil, err
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, errors.Join(
			fmt.Errorf("dial unix socket returned %T", conn),
			conn.Close(),
		)
	}
	return unixConn, nil
}

// Bridge pipes rw to and from one connection to a local session's socket,
// proxying the socket protocol verbatim (JSON handshake and frames alike) so a
// remote client can drive the session over any byte transport. Bridge takes
// ownership of rw and always closes it before returning, which lets both copy
// directions be unblocked and joined. It returns once the session side closes
// the connection, rw fails, or ctx is canceled; when rw reaches EOF first, the
// connection's write side is half-closed so the session observes the EOF while
// its remaining output still drains to rw.
func Bridge(ctx context.Context, id string, rw io.ReadWriteCloser) (retErr error) {
	conn, err := dialSession(ctx, id)
	if err != nil {
		return errors.Join(
			fmt.Errorf("bridge: dial: %w", err),
			wrapBridgeError("close caller stream", rw.Close()),
		)
	}

	var closeOnce sync.Once
	var connCloseErr error
	closeConnNow := func() {
		closeOnce.Do(func() {
			connCloseErr = conn.Close()
		})
	}
	closeConn := func() error {
		closeConnNow()
		return connCloseErr
	}
	defer func() {
		retErr = errors.Join(retErr, wrapBridgeError("close session connection", closeConn()))
	}()

	stopCancellation := context.AfterFunc(ctx, closeConnNow)
	defer stopCancellation()

	// shuttingDown marks errors induced by Bridge's own teardown (closing rw to
	// unblock a pending inbound read) so they are not reported as failures. It
	// is checked before the conn close/half-close, which the session-side EOF
	// that starts teardown can only follow, so a genuine inbound error is never
	// suppressed.
	var shuttingDown atomic.Bool
	inboundDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(conn, rw)
		if shuttingDown.Load() {
			copyErr = nil
		}
		var shutdownErr error
		if copyErr != nil {
			// A broken caller stream can no longer drive the session; close the
			// connection outright so the outbound copy unblocks even while the
			// session stays open.
			shutdownErr = wrapBridgeError("close session connection", closeConn())
		} else {
			// Half-close so the session observes the caller's EOF while its
			// remaining output still drains below.
			shutdownErr = wrapBridgeCloseWriteError(conn.CloseWrite())
		}
		inboundDone <- errors.Join(copyErr, shutdownErr)
	}()

	_, outErr := io.Copy(rw, conn)
	shuttingDown.Store(true)
	rwCloseErr := wrapBridgeError("close caller stream", rw.Close())
	inErr := <-inboundDone

	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(ctxErr, rwCloseErr, inErr)
	}
	// A genuine inbound error closed the connection itself, making any outbound
	// error a teardown artifact, so the inbound error takes precedence.
	if inErr != nil {
		return errors.Join(fmt.Errorf("bridge: %w", inErr), rwCloseErr)
	}
	if outErr != nil {
		return errors.Join(fmt.Errorf("bridge: %w", outErr), rwCloseErr)
	}
	return rwCloseErr
}
func wrapBridgeError(operation string, err error) error {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return fmt.Errorf("bridge: %s: %w", operation, err)
}

func wrapBridgeCloseWriteError(err error) error {
	if errors.Is(err, syscall.ENOTCONN) {
		return nil
	}
	return wrapBridgeError("close session write side", err)
}
