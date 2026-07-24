package e2e

// These tests validate the '### High-Level Constraints' of intent/bgx.md that
// every command's failure path emits a single JSON object with an error code
// alongside the message, always on stderr and never stdout — including
// commands (history, attach) whose success output is not JSON, and flag parse
// failures that would otherwise print plain-text usage help.

import "testing"

func TestErrorsAreJSONOnStderrWithCodes(t *testing.T) {
	dir := runDir(t)

	cases := []struct {
		name     string
		args     []string
		wantCode string
	}{
		{"run missing id", []string{"run"}, "invalid_argument"},
		{"run missing command", []string{"run", "no-command"}, "invalid_argument"},
		{"info missing id", []string{"info"}, "invalid_argument"},
		{"wait unknown session", []string{"wait", "ghost"}, "not_found"},
		{"kill unknown session", []string{"kill", "ghost"}, "not_found"},
		{"send unknown session", []string{"send", "ghost", "hi"}, "not_found"},
		{"history unknown session", []string{"history", "ghost"}, "not_found"},
		{"attach unknown session", []string{"attach", "ghost"}, "session_not_found"},
		{"list invalid metadata filter", []string{"list", "--metadata", "no-equals"}, "invalid_argument"},
		{"unknown flag usage error", []string{"kill", "--bogus-flag", "x"}, "invalid_argument"},
		{"unknown command", []string{"bogus-command"}, "invalid_argument"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := bgxIn(t, dir, tc.args...)
			if res.exitCode == 0 {
				t.Fatalf("bgx %v succeeded, want failure", tc.args)
			}
			if res.stdout != "" {
				t.Fatalf("bgx %v wrote %q to stdout, want errors only on stderr", tc.args, res.stdout)
			}
			m := decodeJSON(t, res.stderr)
			if msg, ok := m["error"].(string); !ok || msg == "" {
				t.Fatalf("bgx %v stderr = %q, want JSON error message", tc.args, res.stderr)
			}
			if m["code"] != tc.wantCode {
				t.Fatalf("bgx %v error code = %v, want %q", tc.args, m["code"], tc.wantCode)
			}
			if m["source"] != "bgx" {
				t.Fatalf("bgx %v error source = %v, want %q", tc.args, m["source"], "bgx")
			}
		})
	}
}
