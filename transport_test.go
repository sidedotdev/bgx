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
	os.Setenv("XDG_RUNTIME_DIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
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
	t.Cleanup(func() { ln.Close() })
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
	s.closeOnce.Do(func() {
		if c, ok := s.Reader.(io.Closer); ok {
			c.Close()
		}
		close(s.closed)
	})
	return nil
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
		defer conn.Close()
		line, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			serverErr <- fmt.Errorf("read request: %w", err)
			return
		}
		var req daemon.Request
		if err := json.Unmarshal(line, &req); err != nil {
			serverErr <- fmt.Errorf("decode request %q: %w", line, err)
			return
		}
		if req.Op != "info" {
			serverErr <- fmt.Errorf("request op = %q, want %q", req.Op, "info")
			return
		}
		serverErr <- json.NewEncoder(conn).Encode(daemon.Response{
			OK:   true,
			Info: &daemon.Info{ID: id, Running: true},
		})
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
		conn.Close()
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
		conn.Close()
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
		defer conn.Close()
		got := make([]byte, len(inbound))
		if _, err := io.ReadFull(conn, got); err != nil {
			serverErr <- fmt.Errorf("read inbound: %w", err)
			return
		}
		if !bytes.Equal(got, inbound) {
			serverErr <- fmt.Errorf("inbound = %q, want %q", got, inbound)
			return
		}
		_, err = conn.Write(outbound)
		serverErr <- err
	}()

	inR, inW := io.Pipe()
	defer inW.Close()
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
		defer conn.Close()
		// The caller's EOF must surface as EOF here while the connection still
		// carries the reply back.
		got, err := io.ReadAll(conn)
		if err != nil {
			serverErr <- fmt.Errorf("read inbound: %w", err)
			return
		}
		if string(got) != "request" {
			serverErr <- fmt.Errorf("inbound = %q, want %q", got, "request")
			return
		}
		_, err = conn.Write([]byte("reply"))
		serverErr <- err
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
	go func() {
		conn, err := ln.Accept()
		close(accepted)
		if err != nil {
			close(serverClosed)
			return
		}
		_, _ = io.Copy(io.Discard, conn)
		conn.Close()
		close(serverClosed)
	}()

	inR, inW := io.Pipe()
	defer inW.Close()
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
	defer close(release)
	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		// Drain whatever arrives before the caller stream breaks, then hold
		// the connection open: Bridge itself must unblock its outbound copy.
		_, err = io.Copy(io.Discard, conn)
		serverErr <- err
		<-release
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
	if err := <-serverErr; err != nil {
		t.Fatalf("server: %v", err)
	}
	expectStreamClosed(t, stream)
}

func TestBridgeFailsWhenSessionSocketAbsent(t *testing.T) {
	inR, inW := io.Pipe()
	defer inW.Close()
	stream := newBridgeStream(inR, io.Discard)
	if err := Bridge(context.Background(), "no-such-session", stream); err == nil {
		t.Fatal("Bridge returned nil error for an absent socket")
	}
	expectStreamClosed(t, stream)
}
