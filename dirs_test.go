package bgx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComputeDirCandidatesReportsConfiguredXDGAccessError(t *testing.T) {
	root := t.TempDir()
	blockingFile := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockingFile, []byte("block directory creation"), 0o600); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}

	stateDir := filepath.Join(blockingFile, "bgx")
	wantErr := usableDir(stateDir)
	if wantErr == nil {
		t.Fatal("configured XDG state directory unexpectedly usable")
	}

	fallbackDir := filepath.Join(root, "fallback")
	result := computeDirCandidates([]dirCandidate{
		{name: "$XDG_STATE_HOME", path: stateDir},
		{name: "$HOME/.bgx", path: fallbackDir},
	})

	if result.err != nil {
		t.Fatalf("compute directory candidates: %v", result.err)
	}
	if result.base != fallbackDir {
		t.Fatalf("resolved base = %q, want %q", result.base, fallbackDir)
	}
	for _, want := range []string{"$XDG_STATE_HOME", stateDir, wantErr.Error(), "$HOME/.bgx", fallbackDir} {
		if !strings.Contains(result.notice, want) {
			t.Errorf("fallback notice %q does not contain %q", result.notice, want)
		}
	}
}
