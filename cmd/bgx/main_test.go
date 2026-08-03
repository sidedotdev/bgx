package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sidedotdev/bgx/internal/cli"
)

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "success", want: 0},
		{name: "generic failure", err: errors.New("failed"), want: 1},
		{name: "session status", err: &cli.ExitError{Code: 23}, want: 23},
		{
			name: "wrapped session status",
			err:  fmt.Errorf("wait: %w", &cli.ExitError{Code: 42}),
			want: 42,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := exitCode(test.err); got != test.want {
				t.Fatalf("exitCode(%v) = %d, want %d", test.err, got, test.want)
			}
		})
	}
}
