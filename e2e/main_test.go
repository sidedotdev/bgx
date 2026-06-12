package e2e

import (
	"bytes"
	"errors"
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
func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "bgx-e2e")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	binPath = filepath.Join(dir, "bgx")
	build := exec.Command("go", "build", "-o", binPath, ".")
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
