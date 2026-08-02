package bgx

import (
	"context"
	"os"
	"sync"

	cli "github.com/urfave/cli/v3"
)

// bridgeAction forwards one raw connection to the session's socket over the
// process's stdin/stdout, letting a remote `bgx attach --via/--ssh` use this
// process as its transport. Bytes are proxied verbatim: the remote client
// drives the JSON attach handshake and frame protocol end to end.
func bridgeAction(ctx context.Context, cmd *cli.Command) error {
	id := cmd.Args().First()
	if id == "" {
		return failJSON(codeInvalidArgument, "bridge: an id is required")
	}
	info, ok := liveInfo(id)
	if !ok || !info.Running {
		return failSessionUnavailable("bridge", id, ok && !info.Running)
	}
	if err := Bridge(ctx, id, newStdioStream()); err != nil {
		// The session can end between the liveness check and the dial.
		if _, stillLive := liveInfo(id); !stillLive {
			return failSessionUnavailable("bridge", id, false)
		}
		return failJSON(codeBridgeFailed, "%v", err)
	}
	return nil
}

// stdioStream exposes the process's stdin/stdout as the single caller stream
// Bridge proxies to the session socket.
type stdioStream struct {
	closeOnce sync.Once
	closeErr  error
}

func newStdioStream() *stdioStream { return &stdioStream{} }

func (s *stdioStream) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (s *stdioStream) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

// Close closes stdin first so Bridge's pending inbound read unblocks.
func (s *stdioStream) Close() error {
	s.closeOnce.Do(func() {
		err := os.Stdin.Close()
		if cerr := os.Stdout.Close(); err == nil {
			err = cerr
		}
		s.closeErr = err
	})
	return s.closeErr
}
