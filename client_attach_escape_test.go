package bgx_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	bgx "github.com/sidedotdev/bgx"
	"github.com/sidedotdev/bgx/daemon"
)

type delayedEscapeInput struct {
	delay   time.Duration
	readyAt time.Time
	step    int
}

func (r *delayedEscapeInput) Read(p []byte) (int, error) {
	return r.ReadContext(context.Background(), p)
}

func (r *delayedEscapeInput) ReadContext(ctx context.Context, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	switch r.step {
	case 0:
		r.step++
		r.readyAt = time.Now().Add(r.delay)
		p[0] = 0x1b
		return 1, nil
	case 1:
		timer := time.NewTimer(time.Until(r.readyAt))
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-timer.C:
			r.step++
			return copy(p, "[92;5u"), nil
		}
	default:
		p[0] = 0x1c
		return 1, nil
	}
}

func capturedAttachInput(t *testing.T, stream *closeEOFStream) []byte {
	t.Helper()
	stream.mu.Lock()
	written := append([]byte(nil), stream.writes.Bytes()...)
	stream.mu.Unlock()
	lineEnd := bytes.IndexByte(written, '\n')
	if lineEnd < 0 {
		t.Fatal("missing attach request")
	}
	reader := bufio.NewReader(bytes.NewReader(written[lineEnd+1:]))
	var input []byte
	for {
		tag, payload, err := daemon.ReadFrame(reader)
		if errors.Is(err, io.EOF) {
			return input
		}
		if err != nil {
			t.Fatalf("read client frame: %v", err)
		}
		if tag == daemon.FrameInput {
			input = append(input, payload...)
		}
	}
}

func TestClientAttachEscapeTimeout(t *testing.T) {
	for _, delay := range []time.Duration{0, 10 * time.Millisecond, 24 * time.Millisecond, 26 * time.Millisecond, 50 * time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				stream := newCloseEOFStream()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				result := make(chan error, 1)
				go func() {
					result <- bgx.NewClient(func(context.Context) (io.ReadWriteCloser, error) {
						return stream, nil
					}).Attach(ctx, newTestTerminal(&delayedEscapeInput{delay: delay}))
				}()
				synctest.Wait()
				time.Sleep(24 * time.Millisecond)
				synctest.Wait()
				if got := capturedAttachInput(t, stream); len(got) != 0 {
					t.Fatalf("forwarded Escape before timeout: %q", got)
				}
				time.Sleep(time.Millisecond)
				synctest.Wait()
				want := ""
				if delay > 25*time.Millisecond {
					want = "\x1b"
				}
				if got := string(capturedAttachInput(t, stream)); got != want {
					t.Fatalf("input at Escape timeout = %q, want %q", got, want)
				}
				if err := <-result; err != nil {
					t.Fatalf("Attach: %v", err)
				}
				if delay > 25*time.Millisecond {
					want = "\x1b[92;5u"
				}
				if got := string(capturedAttachInput(t, stream)); got != want {
					t.Fatalf("final input = %q, want %q", got, want)
				}
			})
		})
	}
}

func TestClientAttachFlushesPendingEscapeAtInputEOF(t *testing.T) {
	stream := newCloseEOFStream()
	err := bgx.NewClient(func(context.Context) (io.ReadWriteCloser, error) {
		return stream, nil
	}).Attach(context.Background(), newTestTerminal(bytes.NewReader([]byte("hello\x1b"))))
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if got := string(capturedAttachInput(t, stream)); got != "hello\x1b" {
		t.Fatalf("forwarded input = %q, want hello followed by Escape", got)
	}
}

func TestClientAttachRawDetachAfterEscapeOrBatchedInput(t *testing.T) {
	for _, chunks := range [][]string{{"\x1b", "\x1c"}, {"hello\x1c"}} {
		t.Run(chunks[0], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var readers []io.Reader
				for _, chunk := range chunks {
					readers = append(readers, bytes.NewReader([]byte(chunk)))
				}
				stream := newCloseEOFStream()
				err := bgx.NewClient(func(context.Context) (io.ReadWriteCloser, error) {
					return stream, nil
				}).Attach(context.Background(), newTestTerminal(io.MultiReader(readers...)))
				if err != nil {
					t.Fatalf("Attach: %v", err)
				}
				if got := capturedAttachInput(t, stream); len(got) != 0 {
					t.Fatalf("forwarded detach input: %q", got)
				}
				stream.mu.Lock()
				written := append([]byte(nil), stream.writes.Bytes()...)
				stream.mu.Unlock()
				frames := bufio.NewReader(bytes.NewReader(written[bytes.IndexByte(written, '\n')+1:]))
				if tag, _, err := daemon.ReadFrame(frames); err != nil || tag != daemon.FrameResize {
					t.Fatalf("first frame = %v, %v; want resize", tag, err)
				}
				if tag, _, err := daemon.ReadFrame(frames); err != nil || tag != daemon.FrameDetach {
					t.Fatalf("second frame = %v, %v; want detach", tag, err)
				}
			})
		})
	}
}
