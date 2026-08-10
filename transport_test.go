package bgx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sidedotdev/bgx/daemon"
)

// TestMain first lets the test binary act as its own daemon host, exactly like
// an embedding binary would, so Start's re-exec path is exercised in-process
// tests. It then points base-directory resolution at a short-lived directory
// under /tmp before anything memoizes it, keeping unit-test sockets isolated
// and within the unix sun_path length limit (unlike t.TempDir on macOS).
func TestMain(m *testing.M) {
	InterceptDaemon()
	dir, err := os.MkdirTemp("/tmp", "bgx-lib-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "make test runtime dir:", err)
		os.Exit(1)
	}
	if err := os.Setenv("XDG_RUNTIME_DIR", dir); err != nil {
		fmt.Fprintln(os.Stderr, "set test runtime dir:", err)
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			fmt.Fprintln(os.Stderr, "remove test runtime dir:", removeErr)
		}
		os.Exit(1)
	}
	if err := os.Setenv("XDG_STATE_HOME", dir); err != nil {
		fmt.Fprintln(os.Stderr, "set test state dir:", err)
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			fmt.Fprintln(os.Stderr, "remove test state dir:", removeErr)
		}
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "remove test runtime dir:", err)
		code = 1
	}
	os.Exit(code)
}

// listenFakeSession stands in for a session daemon on the socket path Dial and
// Bridge resolve for id.
func listenFakeSession(t *testing.T, id string) net.Listener {
	t.Helper()
	path := socketPath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("make socket dir: %v", err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close listener: %v", err)
		}
	})
	return ln
}

// bridgeStream pairs a readable and a writable end into the caller stream
// Bridge consumes, recording the close Bridge owes it.
type bridgeStream struct {
	io.Reader
	io.Writer
	closeOnce sync.Once
	closed    chan struct{}
}

func newBridgeStream(r io.Reader, w io.Writer) *bridgeStream {
	return &bridgeStream{Reader: r, Writer: w, closed: make(chan struct{})}
}

func (s *bridgeStream) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		if c, ok := s.Reader.(io.Closer); ok {
			closeErr = c.Close()
		}
		close(s.closed)
	})
	return closeErr
}

// expectStreamClosed asserts Bridge closed its stream before returning, which
// is what unblocks and joins a still-pending inbound copy.
func expectStreamClosed(t *testing.T, s *bridgeStream) {
	t.Helper()
	select {
	case <-s.closed:
	default:
		t.Fatal("bridge stream was not closed")
	}
}

func TestDialServesAsLocalDialer(t *testing.T) {
	const id = "dial-client"
	ln := listenFakeSession(t, id)

	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}

		line, requestErr := bufio.NewReader(conn).ReadBytes('\n')
		if requestErr == nil {
			var req daemon.Request
			if err := json.Unmarshal(line, &req); err != nil {
				requestErr = fmt.Errorf("decode request %q: %w", line, err)
			} else if req.Op != "info" {
				requestErr = fmt.Errorf("request op = %q, want %q", req.Op, "info")
			} else if err := json.NewEncoder(conn).Encode(daemon.Response{
				OK:   true,
				Info: &daemon.Info{ID: id, Running: true},
			}); err != nil {
				requestErr = err
			}
		} else {
			requestErr = fmt.Errorf("read request: %w", requestErr)
		}

		closeErr := conn.Close()
		serverErr <- errors.Join(requestErr, closeErr)
	}()

	client := NewClient(func(ctx context.Context) (io.ReadWriteCloser, error) {
		return Dial(ctx, id)
	})
	info, err := client.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.ID != id || !info.Running {
		t.Fatalf("info = %+v, want id=%q running", info, id)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func TestDialFailsWhenSessionSocketAbsent(t *testing.T) {
	if conn, err := Dial(context.Background(), "no-such-session"); err == nil {
		if closeErr := conn.Close(); closeErr != nil {
			t.Fatalf("close unexpected connection: %v", closeErr)
		}
		t.Fatal("Dial returned nil error for an absent socket")
	}
}

func TestDialHonorsCanceledContext(t *testing.T) {
	const id = "dial-cancel"
	listenFakeSession(t, id)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := Dial(ctx, id)
	if err == nil {
		if closeErr := conn.Close(); closeErr != nil {
			t.Fatalf("close unexpected connection: %v", closeErr)
		}
		t.Fatal("Dial returned nil error with a canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Dial error = %v, want context.Canceled", err)
	}
}

func TestBridgeProxiesBytesVerbatimUntilSessionCloses(t *testing.T) {
	const id = "bridge-verbatim"
	ln := listenFakeSession(t, id)

	// Arbitrary binary payloads shaped like the attach handshake plus frames:
	// the bridge must not interpret or alter them in either direction.
	inbound := []byte("{\"op\":\"attach\"}\n\x00\x00\x00\x00\x05hello")
	outbound := []byte("{\"ok\":true}\n\x01\x00\x00\x00\x03out\xff")

	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}

		got := make([]byte, len(inbound))
		var serveErr error
		if _, err := io.ReadFull(conn, got); err != nil {
			serveErr = fmt.Errorf("read inbound: %w", err)
		} else if !bytes.Equal(got, inbound) {
			serveErr = fmt.Errorf("inbound = %q, want %q", got, inbound)
		} else {
			_, serveErr = conn.Write(outbound)
		}

		closeErr := conn.Close()
		serverErr <- errors.Join(serveErr, closeErr)
	}()

	inR, inW := io.Pipe()
	t.Cleanup(func() {
		if err := inW.Close(); err != nil {
			t.Errorf("close inbound writer: %v", err)
		}
	})
	var out bytes.Buffer
	stream := newBridgeStream(inR, &out)
	done := make(chan error, 1)
	go func() {
		done <- Bridge(context.Background(), id, stream)
	}()

	if _, err := inW.Write(inbound); err != nil {
		t.Fatalf("write inbound: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Bridge: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Bridge did not return after the session closed")
	}
	if !bytes.Equal(out.Bytes(), outbound) {
		t.Fatalf("outbound = %q, want %q", out.Bytes(), outbound)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server: %v", err)
	}
	expectStreamClosed(t, stream)
}

func TestBridgeHalfClosesSessionWriteSideOnCallerEOF(t *testing.T) {
	const id = "bridge-halfclose"
	ln := listenFakeSession(t, id)

	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}

		// The caller's EOF must surface as EOF here while the connection still
		// carries the reply back.
		got, serveErr := io.ReadAll(conn)
		if serveErr != nil {
			serveErr = fmt.Errorf("read inbound: %w", serveErr)
		} else if string(got) != "request" {
			serveErr = fmt.Errorf("inbound = %q, want %q", got, "request")
		} else {
			_, serveErr = conn.Write([]byte("reply"))
		}

		closeErr := conn.Close()
		serverErr <- errors.Join(serveErr, closeErr)
	}()

	inR, inW := io.Pipe()
	var out bytes.Buffer
	stream := newBridgeStream(inR, &out)
	done := make(chan error, 1)
	go func() {
		done <- Bridge(context.Background(), id, stream)
	}()

	if _, err := inW.Write([]byte("request")); err != nil {
		t.Fatalf("write inbound: %v", err)
	}
	if err := inW.Close(); err != nil {
		t.Fatalf("close inbound: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Bridge: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Bridge did not return after the session closed")
	}
	if got := out.String(); got != "reply" {
		t.Fatalf("outbound = %q, want %q", got, "reply")
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server: %v", err)
	}
	expectStreamClosed(t, stream)
}

func TestBridgeCancellationClosesConnection(t *testing.T) {
	const id = "bridge-cancel"
	ln := listenFakeSession(t, id)

	accepted := make(chan struct{})
	serverClosed := make(chan struct{})
	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		close(accepted)
		if err != nil {
			serverErr <- err
			close(serverClosed)
			return
		}
		_, copyErr := io.Copy(io.Discard, conn)
		closeErr := conn.Close()
		serverErr <- errors.Join(copyErr, closeErr)
		close(serverClosed)
	}()

	inR, inW := io.Pipe()
	t.Cleanup(func() {
		if err := inW.Close(); err != nil {
			t.Errorf("close inbound writer: %v", err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newBridgeStream(inR, io.Discard)
	done := make(chan error, 1)
	go func() {
		done <- Bridge(ctx, id, stream)
	}()

	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("session side never accepted the bridge connection")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Bridge error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Bridge did not return after cancellation")
	}
	select {
	case <-serverClosed:
		if err := <-serverErr; err != nil && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("session server: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bridge connection was not closed after cancellation")
	}
	expectStreamClosed(t, stream)
}

// failingReader simulates a caller-side transport breaking mid-bridge.
type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestBridgePropagatesCallerReadError(t *testing.T) {
	const id = "bridge-read-error"
	ln := listenFakeSession(t, id)

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseServer := func() {
		releaseOnce.Do(func() {
			close(release)
		})
	}
	defer releaseServer()

	serverErr := make(chan error, 1)
	serverCloseErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			serverCloseErr <- nil
			return
		}
		// Drain whatever arrives before the caller stream breaks, then hold
		// the connection open: Bridge itself must unblock its outbound copy.
		_, err = io.Copy(io.Discard, conn)
		serverErr <- err
		<-release
		serverCloseErr <- conn.Close()
	}()

	want := errors.New("caller stream broke")
	stream := newBridgeStream(failingReader{err: want}, io.Discard)
	done := make(chan error, 1)
	go func() {
		done <- Bridge(context.Background(), id, stream)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Fatalf("Bridge error = %v, want %v", err, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Bridge did not return while the session side stayed open")
	}

	serveErr := <-serverErr
	releaseServer()
	closeErr := <-serverCloseErr
	if err := errors.Join(serveErr, closeErr); err != nil {
		t.Fatalf("server: %v", err)
	}
	expectStreamClosed(t, stream)
}

func TestBridgeFailsWhenSessionSocketAbsent(t *testing.T) {
	inR, inW := io.Pipe()
	t.Cleanup(func() {
		if err := inW.Close(); err != nil {
			t.Errorf("close inbound writer: %v", err)
		}
	})
	stream := newBridgeStream(inR, io.Discard)
	if err := Bridge(context.Background(), "no-such-session", stream); err == nil {
		t.Fatal("Bridge returned nil error for an absent socket")
	}
	expectStreamClosed(t, stream)
}

type closeFailingBridgeStream struct {
	*bridgeStream
	err error
}

func (s *closeFailingBridgeStream) Close() error {
	return errors.Join(s.bridgeStream.Close(), s.err)
}

func TestBridgeAggregatesDialAndCallerCloseErrors(t *testing.T) {
	closeErr := errors.New("close caller stream")
	inR, inW := io.Pipe()
	t.Cleanup(func() {
		if err := inW.Close(); err != nil {
			t.Errorf("close inbound writer: %v", err)
		}
	})
	stream := &closeFailingBridgeStream{
		bridgeStream: newBridgeStream(inR, io.Discard),
		err:          closeErr,
	}

	err := Bridge(context.Background(), "no-such-session", stream)
	if err == nil {
		t.Fatal("Bridge returned nil error for an absent socket")
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("Bridge error = %v, want caller close error", err)
	}
	expectStreamClosed(t, stream.bridgeStream)
}

func TestBridgeReturnsCallerCloseErrorAfterProxying(t *testing.T) {
	const id = "bridge-close-error"
	ln := listenFakeSession(t, id)

	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		if err := conn.Close(); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	closeErr := errors.New("close caller stream")
	stream := &closeFailingBridgeStream{
		bridgeStream: newBridgeStream(bytes.NewReader(nil), io.Discard),
		err:          closeErr,
	}
	err := Bridge(context.Background(), id, stream)
	if !errors.Is(err, closeErr) {
		t.Fatalf("Bridge error = %v, want caller close error", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("session server: %v", err)
	}
	expectStreamClosed(t, stream.bridgeStream)
}
func TestBridgeRetainsCallerCloseENOTCONN(t *testing.T) {
	inR, inW := io.Pipe()
	t.Cleanup(func() {
		if err := inW.Close(); err != nil {
			t.Errorf("close inbound writer: %v", err)
		}
	})
	stream := &closeFailingBridgeStream{
		bridgeStream: newBridgeStream(inR, io.Discard),
		err:          syscall.ENOTCONN,
	}

	err := Bridge(context.Background(), "no-such-session", stream)
	if !errors.Is(err, syscall.ENOTCONN) {
		t.Fatalf("Bridge error = %v, want caller ENOTCONN", err)
	}
	expectStreamClosed(t, stream.bridgeStream)
}
