package bgx

import (
	"errors"
	"os"
	"testing"
)

func TestProcessTerminalSizeReportsPipeAsUnavailable(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create input pipe: %v", err)
	}
	t.Cleanup(func() {
		if err := input.Close(); err != nil {
			t.Errorf("close input pipe: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Errorf("close input writer: %v", err)
		}
	})

	terminal := &ProcessTerminal{in: input}
	cols, rows, err := terminal.Size()
	if !errors.Is(err, ErrTerminalSizeUnavailable) {
		t.Fatalf("Size error = %v, want ErrTerminalSizeUnavailable", err)
	}
	if cols != 0 || rows != 0 {
		t.Fatalf("Size = %dx%d, want 0x0", cols, rows)
	}
}
