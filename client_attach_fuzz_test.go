package bgx_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	bgx "github.com/sidedotdev/bgx"
	"github.com/sidedotdev/bgx/daemon"
)

type fuzzAttachInput struct {
	prefix   *bytes.Reader
	key      *bytes.Reader
	chunk    int
	finalErr error
	onRead   func() error
	block    bool
}

func (r *fuzzAttachInput) Read(p []byte) (int, error) {
	return r.ReadContext(context.Background(), p)
}

func (r *fuzzAttachInput) ReadContext(ctx context.Context, p []byte) (int, error) {
	if r.onRead != nil {
		onRead := r.onRead
		r.onRead = nil
		if err := onRead(); err != nil {
			return 0, err
		}
	}
	if r.block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	p = p[:min(len(p), r.chunk)]
	if r.prefix.Len() > 0 {
		return r.prefix.Read(p)
	}
	n, err := r.key.Read(p)
	if n > 0 && r.key.Len() == 0 {
		return n, r.finalErr
	}
	return n, err
}

func FuzzClientAttachShutdown(f *testing.F) {
	for schedule := uint8(0); schedule < 4; schedule++ {
		for _, faults := range []uint8{0, 1, 2, 4, 8, 24, 31} {
			f.Add([]byte("hello"), uint8(1), uint8(1), schedule, faults)
		}
	}
	f.Add([]byte{}, uint8(255), uint8(0), uint8(1), uint8(0))
	f.Add([]byte{}, uint8(0), uint8(2), uint8(1), uint8(0))

	f.Fuzz(func(t *testing.T, data []byte, chunk, key, schedule, faults uint8) {
		schedule %= 4
		detaching := schedule < 2
		// Printable input excludes accidental detach keys so forwarding has an
		// independent oracle; key encoding and read boundaries vary separately.
		prefix := append([]byte(nil), data[:min(len(data), 256)]...)
		for i := range prefix {
			prefix[i] = 'a' + prefix[i]%26
		}
		keys := []string{"\x1c", "\x1b[92;5u", "\x1b\x1c"}
		input := &fuzzAttachInput{
			prefix:   bytes.NewReader(prefix),
			key:      bytes.NewReader([]byte(keys[int(key)%len(keys)])),
			chunk:    int(chunk) + 1,
			finalErr: io.EOF,
			block:    !detaching,
		}

		frameFailure := errors.New("frame read failed")
		closeFailure := errors.New("stream close failed")
		restoreFailure := errors.New("terminal restore failed")
		inputFailure := errors.New("terminal read failed")
		base := newCloseEOFStream()
		if faults&1 != 0 {
			base.readErr = frameFailure
		}
		var stream io.ReadWriteCloser = base
		if schedule == 1 {
			stream = &peerDetachEOFStream{
				closeEOFStream: base,
				writeReleased:  make(chan struct{}),
			}
		}
		if faults&2 != 0 {
			stream = &closeErrorStream{ReadWriteCloser: stream, err: closeFailure}
		}
		terminal := &failingTerminal{testTerminal: newTestTerminal(input)}
		if faults&4 != 0 {
			terminal.restoreErr = restoreFailure
		}
		if faults&8 != 0 {
			input.finalErr = inputFailure
			if faults&16 != 0 {
				input.finalErr = errors.Join(io.EOF, inputFailure)
			}
		}

		watchdog, stopWatchdog := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopWatchdog()
		ctx, cancel := context.WithCancel(watchdog)
		defer cancel()
		switch schedule {
		case 2:
			input.onRead = base.Close
		case 3:
			input.onRead = func() error {
				cancel()
				return nil
			}
		}
		err := bgx.NewClient(func(context.Context) (io.ReadWriteCloser, error) {
			return stream, nil
		}).Attach(ctx, terminal)
		if watchdog.Err() != nil {
			t.Fatalf("attach exceeded its shutdown deadline: %v", err)
		}

		wantError := false
		for _, check := range []struct {
			err  error
			want bool
		}{
			{frameFailure, faults&1 != 0},
			{closeFailure, faults&2 != 0},
			{restoreFailure, faults&4 != 0},
			{inputFailure, detaching && faults&8 != 0},
			{io.EOF, schedule == 2},
			{context.Canceled, schedule == 3},
		} {
			wantError = wantError || check.want
			if errors.Is(err, check.err) != check.want {
				t.Errorf("Attach error = %v; want errors.Is(%v) = %v", err, check.err, check.want)
			}
		}
		if !wantError && err != nil {
			t.Errorf("Attach error = %v, want nil", err)
		}
		_, entered, restored := terminal.snapshot()
		if !entered || !restored {
			t.Errorf("terminal raw state: entered %v, restored %v", entered, restored)
		}
		select {
		case <-base.closed:
		default:
			t.Error("attach returned without closing its stream")
		}

		base.mu.Lock()
		written := append([]byte(nil), base.writes.Bytes()...)
		base.mu.Unlock()
		lineEnd := bytes.IndexByte(written, '\n')
		if lineEnd < 0 {
			t.Fatal("missing attach request")
		}
		frames := bufio.NewReader(bytes.NewReader(written[lineEnd+1:]))
		var forwarded []byte
		detaches := 0
		for {
			tag, payload, err := daemon.ReadFrame(frames)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("decode client frame: %v", err)
			}
			switch tag {
			case daemon.FrameInput:
				forwarded = append(forwarded, payload...)
			case daemon.FrameDetach:
				detaches++
			}
		}
		if detaching {
			if detaches != 1 || !bytes.Equal(forwarded, prefix) {
				t.Errorf("forwarded %q with %d detach frames, want %q and one detach", forwarded, detaches, prefix)
			}
		} else if detaches != 0 || len(forwarded) != 0 {
			t.Errorf("blocked input forwarded %q with %d detach frames", forwarded, detaches)
		}
	})
}
