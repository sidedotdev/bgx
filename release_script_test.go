package bgx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func runReleaseScript(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	binDir := t.TempDir()
	commandLog := filepath.Join(t.TempDir(), "commands.log")

	writeExecutable(t, filepath.Join(binDir, "git"), `#!/bin/sh
printf 'git %s\n' "$*" >>"$COMMAND_LOG"
if [ -n "${MOCK_GIT_FAIL:-}" ] && printf '%s\n' "$*" | grep -qF "$MOCK_GIT_FAIL"; then
	exit 42
fi
case "$*" in
	*"tag --list"*)
		printf '%s\n' "${MOCK_LOCAL_TAG:-}"
		;;
	*rev-parse*"--verify --quiet refs/tags/"*"^{commit}"*)
		[ "${MOCK_LOCAL_TAG_EXISTS:-false}" = true ] || exit 1
		printf '%s\n' "${MOCK_SHA:-0123456789abcdef}"
		;;
	*ls-remote*)
		if [ "${MOCK_REMOTE_TAG_EXISTS:-false}" = true ]; then
			printf '%s\t%s\n' "${MOCK_REMOTE_SHA:-${MOCK_SHA:-0123456789abcdef}}" "${MOCK_REMOTE_TAG_REF:-refs/tags/v1.2.3}"
		fi
		;;
	*rev-parse*"^{commit}"*)
		printf '%s\n' "${MOCK_SHA:-0123456789abcdef}"
		;;
esac
`)

	writeExecutable(t, filepath.Join(binDir, "gh"), `#!/bin/sh
printf 'gh %s\n' "$*" >>"$COMMAND_LOG"
case "$*" in
	*"actions/runs?"*)
		printf '%s\n' "${MOCK_RUN_LIST:-123}"
		;;
	"release view "*"--json isPrerelease"*)
		if [ -n "${MOCK_PRERELEASE:-}" ]; then
			printf '%s\n' "$MOCK_PRERELEASE"
		else
			case "${MOCK_RELEASE_STATE:-prerelease}" in
				missing) exit 1 ;;
				prerelease) printf 'true\n' ;;
				complete) printf 'false\n' ;;
			esac
		fi
		;;
	"release view "*"--json assets"*)
		printf '%s\n' "${MOCK_ASSETS:-bgx-linux-amd64
bgx-linux-arm64
bgx-darwin-arm64
bgx-darwin-amd64}"
		;;
	"run view "*"--json status,conclusion"*)
		printf '%s\n' "${MOCK_RUN_RESULT:-completed	failure}"
		;;
	"run watch "*)
		if grep -q '^gh run rerun ' "$COMMAND_LOG"; then
			exit "${MOCK_RETRY_EXIT:-0}"
		fi
		exit "${MOCK_WATCH_EXIT:-0}"
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
func TestReleaseScriptPreparesFreshRelease(t *testing.T) {
	t.Setenv("MOCK_RELEASE_STATE", "missing")

	output, stderr, err := runReleaseScript(t, "v1.2.3")
	if err != nil {
		t.Fatalf("release script failed: %v\nstderr:\n%s", err, stderr)
	}

	for _, command := range []string{
		"tag v1.2.3 main",
		"push origin refs/tags/v1.2.3",
		"release create v1.2.3 --verify-tag --prerelease",
		"run watch 123 --exit-status",
		"release edit v1.2.3 --prerelease=false --latest",
	} {
		if !strings.Contains(output, command) {
			t.Errorf("missing command %q in:\n%s", command, output)
		}
	}
}
func TestReleaseScriptResumesIncompleteRelease(t *testing.T) {
	tests := []struct {
		name         string
		localTag     bool
		remoteTag    bool
		releaseState string
		want         []string
		doNotWant    []string
	}{
		{
			name:         "pushes existing local tag",
			localTag:     true,
			releaseState: "missing",
			want: []string{
				"push origin refs/tags/v1.2.3",
				"release create v1.2.3 --verify-tag --prerelease",
			},
			doNotWant: []string{"tag v1.2.3 main"},
		},
		{
			name:         "fetches existing remote tag",
			remoteTag:    true,
			releaseState: "missing",
			want: []string{
				"fetch origin refs/tags/v1.2.3:refs/tags/v1.2.3",
				"release create v1.2.3 --verify-tag --prerelease",
			},
			doNotWant: []string{
				"tag v1.2.3 main",
				"push origin refs/tags/v1.2.3",
			},
		},
		{
			name:         "continues existing prerelease",
			localTag:     true,
			remoteTag:    true,
			releaseState: "prerelease",
			want: []string{
				"run watch 123 --exit-status",
				"release edit v1.2.3 --prerelease=false --latest",
			},
			doNotWant: []string{
				"tag v1.2.3 main",
				"push origin refs/tags/v1.2.3",
				"release create v1.2.3",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MOCK_LOCAL_TAG_EXISTS", strings.ToLower(strconv.FormatBool(tt.localTag)))
			t.Setenv("MOCK_REMOTE_TAG_EXISTS", strings.ToLower(strconv.FormatBool(tt.remoteTag)))
			t.Setenv("MOCK_RELEASE_STATE", tt.releaseState)

			output, stderr, err := runReleaseScript(t, "v1.2.3")
			if err != nil {
				t.Fatalf("release script failed: %v\nstderr:\n%s", err, stderr)
			}
			for _, command := range tt.want {
				if !strings.Contains(output, command) {
					t.Errorf("missing command %q in:\n%s", command, output)
				}
			}
			for _, command := range tt.doNotWant {
				if strings.Contains(output, command) {
					t.Errorf("unexpected command %q in:\n%s", command, output)
				}
			}
		})
	}
}
func TestReleaseScriptTreatsCompletedReleaseAsNoOp(t *testing.T) {
	t.Setenv("MOCK_RELEASE_STATE", "complete")

	output, stderr, err := runReleaseScript(t, "v1.2.3")
	if err != nil {
		t.Fatalf("release script failed: %v\nstderr:\n%s", err, stderr)
	}

	for _, command := range []string{
		"tag v1.2.3 main",
		"push origin refs/tags/v1.2.3",
		"release create v1.2.3",
		"run watch",
		"release edit",
	} {
		if strings.Contains(output, command) {
			t.Errorf("completed release ran command %q:\n%s", command, output)
		}
	}
	if !strings.Contains(stderr, "already complete") {
		t.Fatalf("expected completed release message, got:\n%s", stderr)
	}
}
func TestReleaseScriptRefusesDivergentTags(t *testing.T) {
	t.Setenv("MOCK_LOCAL_TAG_EXISTS", "true")
	t.Setenv("MOCK_REMOTE_TAG_EXISTS", "true")
	t.Setenv("MOCK_REMOTE_SHA", "fedcba9876543210")
	t.Setenv("MOCK_RELEASE_STATE", "missing")

	output, stderr, err := runReleaseScript(t, "v1.2.3")
	if err == nil {
		t.Fatal("expected divergent local and remote tags to fail")
	}
	if !strings.Contains(stderr, "refer to different commits") {
		t.Fatalf("unexpected stderr:\n%s", stderr)
	}
	for _, command := range []string{
		"push origin refs/tags/v1.2.3",
		"release create v1.2.3",
		"run watch",
	} {
		if strings.Contains(output, command) {
			t.Errorf("divergent tags ran command %q:\n%s", command, output)
		}
	}
}
func TestReleaseScriptStopsWhenTagPreparationFails(t *testing.T) {
	tests := []struct {
		name       string
		failingGit string
		localTag   bool
		remoteTag  bool
	}{
		{
			name:       "fetch",
			failingGit: "fetch origin",
			remoteTag:  true,
		},
		{
			name:       "tag creation",
			failingGit: "tag v1.2.3 main",
		},
		{
			name:       "created tag resolution",
			failingGit: "rev-parse v1.2.3^{commit}",
		},
		{
			name:       "push",
			failingGit: "push origin refs/tags/v1.2.3",
			localTag:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MOCK_RELEASE_STATE", "missing")
			t.Setenv("MOCK_GIT_FAIL", tt.failingGit)
			t.Setenv("MOCK_LOCAL_TAG_EXISTS", strconv.FormatBool(tt.localTag))
			t.Setenv("MOCK_REMOTE_TAG_EXISTS", strconv.FormatBool(tt.remoteTag))

			output, _, err := runReleaseScript(t, "v1.2.3")
			if err == nil {
				t.Fatalf("expected failed %s command to stop release preparation", tt.failingGit)
			}
			for _, command := range []string{"release create v1.2.3", "run watch"} {
				if strings.Contains(output, command) {
					t.Errorf("failed preparation continued with %q:\n%s", command, output)
				}
			}
		})
	}
}

func TestReleaseScriptRetriesOnlyFailedJobs(t *testing.T) {
	for _, retryFails := range []bool{false, true} {
		t.Run(strconv.FormatBool(retryFails), func(t *testing.T) {
			t.Setenv("MOCK_LOCAL_TAG_EXISTS", "true")
			t.Setenv("MOCK_REMOTE_TAG_EXISTS", "true")
			t.Setenv("MOCK_WATCH_EXIT", "1")
			if retryFails {
				t.Setenv("MOCK_RETRY_EXIT", "1")
			} else {
				t.Setenv("MOCK_RETRY_EXIT", "0")
			}

			output, stderr, err := runReleaseScript(t, "v1.2.3")
			if (err != nil) != retryFails {
				t.Fatalf("release error = %v, want failure %v\n%s", err, retryFails, stderr)
			}
			if strings.Count(output, "gh run rerun ") != 1 ||
				!strings.Contains(output, "gh run rerun 123 --failed\n") {
				t.Fatalf("expected exactly one failed-job-only retry:\n%s", output)
			}
			if strings.Count(output, "gh run watch 123 --exit-status\n") != 2 {
				t.Fatalf("expected to watch the original run and its retry:\n%s", output)
			}
			promoted := strings.Contains(output, "release edit v1.2.3 --prerelease=false --latest")
			if promoted == retryFails {
				t.Fatalf("promoted = %v, retry failed = %v:\n%s", promoted, retryFails, output)
			}
			if retryFails && !strings.Contains(output, "gh run view 123 --log\n") {
				t.Fatalf("missing failure logs:\n%s", output)
			}
		})
	}
}
