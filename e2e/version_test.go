package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	res := bgx(t, "version")
	if res.exitCode != 0 {
		t.Fatalf("version exited %d, stderr: %s", res.exitCode, res.stderr)
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(res.stdout), &out); err != nil {
		t.Fatalf("version output is not valid JSON: %v\noutput: %q", err, res.stdout)
	}

	for _, field := range []string{"version", "socket_dir", "retention_dir"} {
		v, ok := out[field]
		if !ok {
			t.Errorf("version JSON missing field %q", field)
			continue
		}
		if s, ok := v.(string); !ok || s == "" {
			t.Errorf("version JSON field %q should be a non-empty string, got %v", field, v)
		}
	}
}

func TestHelpListsSubcommands(t *testing.T) {
	res := bgx(t, "help")
	if res.exitCode != 0 {
		t.Fatalf("help exited %d, stderr: %s", res.exitCode, res.stderr)
	}

	out := res.stdout + res.stderr
	for _, sub := range []string{"run", "wait", "kill", "history", "attach", "send", "info", "list", "version"} {
		if !strings.Contains(out, sub) {
			t.Errorf("help output should list subcommand %q\noutput: %s", sub, out)
		}
	}
}
