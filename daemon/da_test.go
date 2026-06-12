package daemon

import (
	"bytes"
	"testing"
)

func TestScanDeviceAttributes(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"da1", []byte("\x1b[c"), da1Response},
		{"da1 explicit", []byte("\x1b[0c"), da1Response},
		{"da2", []byte("\x1b[>c"), da2Response},
		{"da2 explicit", []byte("\x1b[>0c"), da2Response},
		{"da response ignored", []byte("\x1b[?62;22c"), nil},
		{"embedded", []byte("foo\x1b[cbar"), da1Response},
		{"none", []byte("plain text"), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scanDeviceAttributes(tt.in)
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("scanDeviceAttributes(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNamespace(t *testing.T) {
	tests := map[string]string{
		"foo":         "",
		"foo/bar":     "foo",
		"foo/bar/baz": "foo",
		"a/b":         "a",
	}
	for id, want := range tests {
		if got := Namespace(id); got != want {
			t.Fatalf("Namespace(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestRetentionPathsSeparateNamespaces(t *testing.T) {
	global := RecordPath("/r", "plain")
	scoped := RecordPath("/r", "proj/task")
	if global == scoped {
		t.Fatalf("expected distinct paths, both %q", global)
	}
	if !bytes.Contains([]byte(global), []byte(globalNamespaceDir)) {
		t.Fatalf("global id not under global namespace dir: %q", global)
	}
}
