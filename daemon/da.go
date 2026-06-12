package daemon

import "bytes"

// Device Attributes (DA) query/response sequences. When no client is attached,
// the daemon answers DA queries on behalf of a real terminal so interactive
// programs (e.g. shells probing terminal capabilities) don't hang waiting for a
// reply. Ported from zmx's respondToDeviceAttributes.
var (
	da1Query         = []byte("\x1b[c")
	da1QueryExplicit = []byte("\x1b[0c")
	da2Query         = []byte("\x1b[>c")
	da2QueryExplicit = []byte("\x1b[>0c")
	da1Response      = []byte("\x1b[?62;22c")
	da2Response      = []byte("\x1b[>1;10;0c")
)

// scanDeviceAttributes returns the bytes to feed back into the PTY in response
// to any DA queries found in data. DA *responses* (which carry '?' after the
// CSI introducer) are deliberately skipped so they are never mistaken for
// queries.
func scanDeviceAttributes(data []byte) []byte {
	var out []byte
	for i := 0; i < len(data); i++ {
		if data[i] != 0x1b || i+1 >= len(data) || data[i+1] != '[' {
			continue
		}
		if i+2 < len(data) && data[i+2] == '?' {
			i += 2
			continue
		}
		rest := data[i:]
		switch {
		case bytes.HasPrefix(rest, da2Query), bytes.HasPrefix(rest, da2QueryExplicit):
			out = append(out, da2Response...)
		case bytes.HasPrefix(rest, da1Query), bytes.HasPrefix(rest, da1QueryExplicit):
			out = append(out, da1Response...)
		}
	}
	return out
}
