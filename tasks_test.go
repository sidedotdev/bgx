package bgx

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

func TestTaskRecipes(t *testing.T) {
	data, err := os.ReadFile("Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}

	recipes := make(map[string][]string)
	var current string
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasSuffix(line, ":") && !strings.HasPrefix(line, "\t") {
			current = strings.TrimSuffix(line, ":")
			continue
		}
		if strings.HasPrefix(line, "\t") && current != "" {
			recipes[current] = append(recipes[current], strings.TrimPrefix(line, "\t"))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("parse Justfile: %v", err)
	}

	want := map[string][]string{
		"build":         {"go build ./cmd/bgx"},
		"install":       {"go install ./cmd/bgx"},
		"release *args": {"./scripts/release.sh {{args}}"},
	}
	for recipe, wantCommands := range want {
		gotCommands, ok := recipes[recipe]
		if !ok {
			t.Errorf("missing %q recipe", recipe)
			continue
		}
		if strings.Join(gotCommands, "\n") != strings.Join(wantCommands, "\n") {
			t.Errorf("%q commands = %q, want %q", recipe, gotCommands, wantCommands)
		}
	}
}
