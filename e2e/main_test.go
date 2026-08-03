package e2e

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// binPath is the path to the bgx binary built once for the whole e2e suite.
var binPath string

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run builds the bgx binary into a temp dir so tests exercise it as a black box.
// BGX_E2E_BIN overrides the build with a prebuilt binary, letting CI point the
// suite at the exact static build it uploads to a release.
func run(m *testing.M) (exitCode int) {
	if prebuilt := os.Getenv("BGX_E2E_BIN"); prebuilt != "" {
		abs, err := filepath.Abs(prebuilt)
		if err != nil {
			panic("failed to resolve BGX_E2E_BIN: " + err.Error())
		}
		binPath = abs
		return m.Run()
	}

	dir, err := os.MkdirTemp("", "bgx-e2e")
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			if _, writeErr := fmt.Fprintf(os.Stderr, "failed to remove E2E build directory %s: %v\n", dir, err); writeErr != nil {
				exitCode = 1
			}
			if exitCode == 0 {
				exitCode = 1
			}
		}
	}()

	binPath = filepath.Join(dir, "bgx")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/bgx")
	build.Dir = ".."
	build.Stderr = os.Stderr
	build.Stdout = os.Stdout
	if err := build.Run(); err != nil {
		panic("failed to build bgx: " + err.Error())
	}

	return m.Run()
}

// result captures the outcome of invoking the bgx binary.
type result struct {
	stdout   string
	stderr   string
	exitCode int
}

// bgx runs the built binary with the given args and captures its output.
func bgx(t *testing.T, args ...string) result {
	t.Helper()

	cmd := exec.Command(binPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			t.Fatalf("failed to run bgx %v: %v", args, err)
		}
	}

	return result{stdout: stdout.String(), stderr: stderr.String(), exitCode: exitCode}
}
