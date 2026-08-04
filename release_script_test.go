package bgx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runReleaseScript(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	binDir := t.TempDir()
	commandLog := filepath.Join(t.TempDir(), "commands.log")

	writeExecutable(t, filepath.Join(binDir, "git"), `#!/bin/sh
printf 'git %s\n' "$*" >>"$COMMAND_LOG"
case "$*" in
	*rev-parse*"^{commit}"*) printf '%s\n' "${MOCK_SHA:-0123456789abcdef}" ;;
	*"tag --list"*) printf '%s\n' "${MOCK_LOCAL_TAG:-}" ;;
esac
`)

	writeExecutable(t, filepath.Join(binDir, "gh"), `#!/bin/sh
printf 'gh %s\n' "$*" >>"$COMMAND_LOG"
case "$*" in
	*"actions/runs?"*)
		printf '%s\n' "${MOCK_RUN_LIST:-123}"
		;;
	"release view "*"--json isPrerelease"*)
		printf '%s\n' "${MOCK_PRERELEASE:-true}"
		;;
	"run view "*"--json status,conclusion"*)
		printf '%s\n' "${MOCK_RUN_RESULT:-completed	failure}"
		;;
esac
`)

	cmd := exec.Command("bash", append([]string{"scripts/release.sh"}, args...)...)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"COMMAND_LOG="+commandLog,
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	logData, readErr := os.ReadFile(commandLog)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read command log: %v", readErr)
	}
	return stdout.String() + string(logData), stderr.String(), err
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write mock executable: %v", err)
	}
}

func TestReleaseScriptListsRunsForSuppliedTag(t *testing.T) {
	output, stderr, err := runReleaseScript(t, "--list", "v1.2.3")
	if err != nil {
		t.Fatalf("release script failed: %v\nstderr:\n%s", err, stderr)
	}

	if !strings.Contains(output, "actions/runs?head_sha=0123456789abcdef") {
		t.Fatalf("expected runs to be queried by resolved tag SHA, got:\n%s", output)
	}
	if strings.Contains(output, "release create") || strings.Contains(output, "git tag v1.2.3") {
		t.Fatalf("list operation modified the release or tag:\n%s", output)
	}
}

func TestReleaseScriptListRequiresRefOrTag(t *testing.T) {
	_, stderr, err := runReleaseScript(t, "--list")
	if err == nil {
		t.Fatal("expected --list without a ref or tag to fail")
	}
	if !strings.Contains(stderr, "--list requires a ref or tag") {
		t.Fatalf("unexpected stderr:\n%s", stderr)
	}
}

func TestReleaseScriptDeletesFailedPrereleaseAndTags(t *testing.T) {
	output, stderr, err := runReleaseScript(t, "--delete", "v1.2.3")
	if err != nil {
		t.Fatalf("release script failed: %v\nstderr:\n%s", err, stderr)
	}

	for _, command := range []string{
		"gh release delete v1.2.3 --yes",
		"push origin --delete refs/tags/v1.2.3",
		"tag --delete v1.2.3",
	} {
		if !strings.Contains(output, command) {
			t.Errorf("missing command %q in:\n%s", command, output)
		}
	}
}

func TestReleaseScriptRefusesToDeleteSuccessfulPrerelease(t *testing.T) {
	t.Setenv("MOCK_RUN_RESULT", "completed\tsuccess")

	output, stderr, err := runReleaseScript(t, "--delete", "v1.2.3")
	if err == nil {
		t.Fatal("expected deletion of a successful prerelease to fail")
	}
	if !strings.Contains(stderr, "workflow run") || !strings.Contains(stderr, "did not fail") {
		t.Fatalf("unexpected stderr:\n%s", stderr)
	}
	if strings.Contains(output, "release delete") || strings.Contains(output, "push origin --delete") {
		t.Fatalf("successful prerelease was modified:\n%s", output)
	}
}

func TestReleaseScriptRefusesToDeleteFullRelease(t *testing.T) {
	t.Setenv("MOCK_PRERELEASE", "false")

	output, stderr, err := runReleaseScript(t, "--delete", "v1.2.3")
	if err == nil {
		t.Fatal("expected deletion of a full release to fail")
	}
	if !strings.Contains(stderr, "is not a prerelease") {
		t.Fatalf("unexpected stderr:\n%s", stderr)
	}
	if strings.Contains(output, "release delete") || strings.Contains(output, "push origin --delete") {
		t.Fatalf("full release was modified:\n%s", output)
	}
}

func TestReleaseScriptDeleteRequiresRefOrTag(t *testing.T) {
	_, stderr, err := runReleaseScript(t, "--delete")
	if err == nil {
		t.Fatal("expected --delete without a ref or tag to fail")
	}
	if !strings.Contains(stderr, "--delete requires a ref or tag") {
		t.Fatalf("unexpected stderr:\n%s", stderr)
	}
}
