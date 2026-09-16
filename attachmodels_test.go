package bgx

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sidedotdev/bgx/vt"
)

// recordingOutput collects everything the models write to the physical
// terminal, in order, across the writeOut and writeBytes paths.
type recordingOutput struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (r *recordingOutput) writeOut(s string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf.WriteString(s)
	return nil
}

func (r *recordingOutput) writeBytes(p []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf.Write(p)
	return nil
}

func (r *recordingOutput) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func newTestModels(t *testing.T, cfg attachConfig) (*attachModels, *recordingOutput) {
	t.Helper()
	term, err := vt.New(vt.DefaultCols, vt.DefaultRows)
	if err != nil {
		t.Fatalf("vt.New: %v", err)
	}
	t.Cleanup(term.Close)
	out := &recordingOutput{}
	models := newAttachModels(term, cfg, out.writeOut, out.writeBytes)
	if err := models.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	return models, out
}

// sessionSnapshot renders the daemon-side snapshot a session in the given
// state would send on attach.
func sessionSnapshot(t *testing.T, sessionOutput string) []byte {
	t.Helper()
	term, err := vt.New(vt.DefaultCols, vt.DefaultRows)
	if err != nil {
		t.Fatalf("vt.New: %v", err)
	}
	defer term.Close()
	if _, err := term.Write([]byte(sessionOutput)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	snapshot, err := term.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return snapshot
}

func TestAttachModelsAutoSelectsPresentationFromSnapshotScreen(t *testing.T) {
	tests := []struct {
		name          string
		sessionOutput string
		want          presentation
		wantRows      uint16
		wantResized   bool
	}{
		{
			name:          "primary screen is presented natively at full height",
			sessionOutput: "shell$ ",
			want:          presentationNative,
			wantRows:      vt.DefaultRows,
		},
		{
			name:          "alternate screen is presented in isolation with a reserved row",
			sessionOutput: "shell$ \x1b[?1049h\x1b[2J\x1b[HTUI",
			want:          presentationIsolated,
			wantRows:      vt.DefaultRows - 1,
			wantResized:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			models, out := newTestModels(t, attachConfig{mode: AttachModeAuto, showDetachInstructions: true})
			if models.presentation != presentationPending {
				t.Fatalf("presentation before first frame = %v, want pending", models.presentation)
			}
			if out.String() != "" {
				t.Fatalf("auto wrote %q before the first frame", out.String())
			}

			resized, err := models.applyOutput(sessionSnapshot(t, tt.sessionOutput))
			if err != nil {
				t.Fatalf("applyOutput: %v", err)
			}
			defer func() {
				if err := models.finish(attachOutcomeAborted); err != nil {
					t.Errorf("finish: %v", err)
				}
			}()
			if models.presentation != tt.want {
				t.Fatalf("presentation = %v, want %v", models.presentation, tt.want)
			}
			if resized != tt.wantResized {
				t.Fatalf("resized = %v, want %v", resized, tt.wantResized)
			}
			rows, err := models.applyResize(vt.DefaultCols, vt.DefaultRows)
			if err != nil {
				t.Fatalf("applyResize: %v", err)
			}
			if rows != tt.wantRows {
				t.Fatalf("advertised rows = %d, want %d", rows, tt.wantRows)
			}
			entered := strings.Contains(out.String(), isolatedEntry)
			if entered != (tt.want == presentationIsolated) {
				t.Fatalf("alternate screen entered = %v for %v presentation; output %q", entered, tt.want, out.String())
			}
		})
	}
}

func TestAttachModelsForcedModesIgnoreSnapshotScreen(t *testing.T) {
	altSnapshot := sessionSnapshot(t, "shell$ \x1b[?1049h\x1b[2J\x1b[HTUI")
	primarySnapshot := sessionSnapshot(t, "shell$ ")

	t.Run("native stays native on the alternate screen", func(t *testing.T) {
		models, out := newTestModels(t, attachConfig{mode: AttachModeNative})
		if _, err := models.applyOutput(altSnapshot); err != nil {
			t.Fatalf("applyOutput: %v", err)
		}
		if models.presentation != presentationNative || models.view != nil {
			t.Fatalf("presentation = %v view=%v, want native without a view", models.presentation, models.view != nil)
		}
		// The session's own alternate-screen entry is forwarded, so the
		// physical terminal follows it and cleanup must leave it again.
		if err := models.finish(attachOutcomeDetached); err != nil {
			t.Fatalf("finish: %v", err)
		}
		if !strings.Contains(out.String(), "\x1b[?1049l"+nativeCleanup) {
			t.Fatalf("output %q lacks alternate-screen exit before native cleanup", out.String())
		}
		if !strings.HasSuffix(out.String(), "\r\nDetached from session\r\n") {
			t.Fatalf("output %q lacks the detach outcome line", out.String())
		}
	})

	t.Run("isolated stays isolated on the primary screen", func(t *testing.T) {
		models, out := newTestModels(t, attachConfig{mode: AttachModeIsolated})
		if !strings.HasPrefix(out.String(), isolatedEntry) {
			t.Fatalf("isolated did not enter the alternate screen up front; output %q", out.String())
		}
		if _, err := models.applyOutput(primarySnapshot); err != nil {
			t.Fatalf("applyOutput: %v", err)
		}
		if models.presentation != presentationIsolated || models.view == nil {
			t.Fatalf("presentation = %v view=%v, want isolated with a view", models.presentation, models.view != nil)
		}
		if err := models.finish(attachOutcomeDetached); err != nil {
			t.Fatalf("finish: %v", err)
		}
		if !strings.HasSuffix(out.String(), "\x1b[?1049l\r\nDetached from session\r\n") {
			t.Fatalf("output %q lacks isolated detach cleanup", out.String())
		}
	})
}

// waitForPaint blocks until the isolated view has completed a repaint
// satisfying ok and returns that paint's body.
func waitForPaint(t *testing.T, out *recordingOutput, ok func(paint string) bool) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := out.String()
		// The trailing element is whatever follows the last completed paint.
		paints := strings.Split(got, "\x1b[?2026l")
		for _, paint := range paints[:len(paints)-1] {
			paint = paint[strings.LastIndex(paint, "\x1b[?2026h"):]
			if ok(paint) {
				return paint
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no matching paint in output %q", got)
		}
		time.Sleep(paintInterval)
	}
}

func TestAttachModelsIsolatedRepaintsOnColumnOnlyResize(t *testing.T) {
	models, out := newTestModels(t, attachConfig{mode: AttachModeIsolated})
	t.Cleanup(func() {
		// Stop the painter before the shared terminal is closed.
		if err := models.finish(attachOutcomeDetached); err != nil {
			t.Errorf("finish: %v", err)
		}
	})
	if _, err := models.applyResize(80, 24); err != nil {
		t.Fatalf("applyResize: %v", err)
	}
	wide := strings.Repeat("W", 60)
	if _, err := models.applyOutput(sessionSnapshot(t, wide)); err != nil {
		t.Fatalf("applyOutput: %v", err)
	}
	before := waitForPaint(t, out, func(paint string) bool {
		return strings.Contains(paint, wide)
	})

	// Only the width changes; the session area height and reserved-row state
	// stay the same, yet the wide line now wraps and must be repainted.
	if _, err := models.applyResize(40, 24); err != nil {
		t.Fatalf("applyResize: %v", err)
	}
	reflowed, err := models.term.DumpScreen()
	if err != nil {
		t.Fatalf("DumpScreen: %v", err)
	}
	after := waitForPaint(t, out, func(paint string) bool {
		return strings.Contains(paint, string(reflowed))
	})
	if after == before {
		t.Fatalf("repaint after column-only resize matched the previous paint: %q", after)
	}
}

func TestAttachModelsPendingCleanupOnlyPrintsOutcome(t *testing.T) {
	models, out := newTestModels(t, attachConfig{mode: AttachModeAuto})
	if err := models.finish(attachOutcomeDisconnected); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if got := out.String(); got != "\r\nDisconnected from session\r\n" {
		t.Fatalf("output = %q, want only the outcome line", got)
	}
}

// withoutPaints strips the isolated view's coalesced repaints from recorded
// output, leaving only what the models wrote directly.
func withoutPaints(output string) string {
	for {
		start := strings.Index(output, "\x1b[?2026h")
		if start < 0 {
			return output
		}
		end := strings.Index(output[start:], "\x1b[?2026l")
		if end < 0 {
			return output[:start]
		}
		output = output[:start] + output[start+end+len("\x1b[?2026l"):]
	}
}

// renderOutput replays everything written to the physical terminal onto a
// fresh emulator and returns its final screen rendering.
func renderOutput(t *testing.T, output string, cols, rows uint16) string {
	t.Helper()
	term, err := vt.New(cols, rows)
	if err != nil {
		t.Fatalf("vt.New: %v", err)
	}
	defer term.Close()
	if _, err := term.Write([]byte(output)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	screen, err := term.DumpScreen()
	if err != nil {
		t.Fatalf("DumpScreen: %v", err)
	}
	return string(screen)
}

func startAutoNative(t *testing.T, cfg attachConfig) (*attachModels, *recordingOutput) {
	t.Helper()
	cfg.mode = AttachModeAuto
	models, out := newTestModels(t, cfg)
	if _, err := models.applyResize(80, 24); err != nil {
		t.Fatalf("applyResize: %v", err)
	}
	if _, err := models.applyOutput(sessionSnapshot(t, "shell$ ")); err != nil {
		t.Fatalf("applyOutput: %v", err)
	}
	if models.presentation != presentationNative {
		t.Fatalf("presentation = %v, want native", models.presentation)
	}
	return models, out
}

func TestAttachModelsAutoFollowsScreenSwitchesWithinOnePayload(t *testing.T) {
	models, out := startAutoNative(t, attachConfig{showDetachInstructions: true})
	t.Cleanup(func() {
		if err := models.finish(attachOutcomeAborted); err != nil {
			t.Errorf("finish: %v", err)
		}
	})
	mark := len(out.String())

	resized, err := models.applyOutput([]byte("ls\r\nfile\r\n\x1b[?1049h\x1b[2J\x1b[HTUI\x1b[?1049lshell$ "))
	if err != nil {
		t.Fatalf("applyOutput: %v", err)
	}
	if !resized {
		t.Fatal("round trip through the alternate screen did not report a size change")
	}
	if models.presentation != presentationNative || models.view != nil {
		t.Fatalf("presentation = %v view=%v, want native without a view", models.presentation, models.view != nil)
	}

	got := out.String()[mark:]
	direct := withoutPaints(got)
	wantPrefix := "ls\r\nfile\r\n" + nativeModeReset + isolatedEntry + "\x1b[?1049l\x1b[r\x1b[2J\x1b[H\x1b[0m"
	if !strings.HasPrefix(direct, wantPrefix) {
		t.Fatalf("direct output %q, want prefix %q", direct, wantPrefix)
	}
	// The alternate-screen content only ever reaches the terminal through the
	// isolated view's paints, never raw.
	if strings.Contains(direct, "TUI") {
		t.Fatalf("alternate-screen bytes were forwarded raw: %q", direct)
	}
	// The whole payload is modelled before the view gets to paint, so the
	// transient alternate screen coalesces into a single isolated paint that
	// still reserved the hint row.
	paint := waitForPaint(t, out, func(paint string) bool { return strings.Contains(paint, "\x1b[1;23r") })
	if !strings.Contains(paint, detachHint) {
		t.Fatalf("isolated paint %q did not draw the hint on the reserved row", paint)
	}
	// The one-time native hint is not re-printed after returning to native.
	if strings.Count(direct, detachHint) != 0 {
		t.Fatalf("native hint re-printed after returning from the alternate screen: %q", direct)
	}
	rendered := renderOutput(t, out.String(), 80, 24)
	for _, want := range []string{"shell$ ls", "file", "shell$ "} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered screen %q lacks %q", rendered, want)
		}
	}
	if strings.Contains(rendered, "TUI") {
		t.Fatalf("rendered screen %q still shows the alternate screen", rendered)
	}
}

func TestAttachModelsAutoFollowsScreenSwitchSplitAcrossFrames(t *testing.T) {
	models, out := startAutoNative(t, attachConfig{showDetachInstructions: true})
	t.Cleanup(func() {
		if err := models.finish(attachOutcomeAborted); err != nil {
			t.Errorf("finish: %v", err)
		}
	})

	mark := len(out.String())
	resized, err := models.applyOutput([]byte("ls\r\n\x1b[?10"))
	if err != nil {
		t.Fatalf("applyOutput: %v", err)
	}
	if resized || models.presentation != presentationNative {
		t.Fatalf("incomplete switch: resized=%v presentation=%v", resized, models.presentation)
	}
	if got := out.String()[mark:]; got != "ls\r\n\x1b[?10" {
		t.Fatalf("native output %q, want the partial sequence forwarded raw", got)
	}

	mark = len(out.String())
	resized, err = models.applyOutput([]byte("49h\x1b[2J\x1b[HTUI"))
	if err != nil {
		t.Fatalf("applyOutput: %v", err)
	}
	if !resized || models.presentation != presentationIsolated || models.view == nil {
		t.Fatalf("completed switch: resized=%v presentation=%v view=%v", resized, models.presentation, models.view != nil)
	}
	if direct := withoutPaints(out.String()[mark:]); direct != nativeModeReset+isolatedEntry {
		t.Fatalf("direct output after switch %q, want only %q", direct, nativeModeReset+isolatedEntry)
	}
	rows, err := models.applyResize(80, 24)
	if err != nil {
		t.Fatalf("applyResize: %v", err)
	}
	if rows != 23 {
		t.Fatalf("advertised rows while isolated = %d, want 23", rows)
	}
	waitForPaint(t, out, func(paint string) bool { return strings.Contains(paint, "TUI") })

	mark = len(out.String())
	resized, err = models.applyOutput([]byte("\x1b[?1049lshell$ "))
	if err != nil {
		t.Fatalf("applyOutput: %v", err)
	}
	if !resized || models.presentation != presentationNative || models.view != nil {
		t.Fatalf("switch back: resized=%v presentation=%v view=%v", resized, models.presentation, models.view != nil)
	}
	direct := withoutPaints(out.String()[mark:])
	if !strings.HasPrefix(direct, "\x1b[?1049l\x1b[r\x1b[2J\x1b[H\x1b[0m") {
		t.Fatalf("direct output after leaving isolation %q, want alternate screen left then repaint", direct)
	}
	if strings.Count(direct, "\x1b[?1049l") != 1 {
		t.Fatalf("session's own alternate-screen exit was forwarded: %q", direct)
	}
	rows, err = models.applyResize(80, 24)
	if err != nil {
		t.Fatalf("applyResize: %v", err)
	}
	if rows != 24 {
		t.Fatalf("advertised rows after returning to native = %d, want 24", rows)
	}
}

func TestAttachModelsAutoResynchronizesAcrossScreens(t *testing.T) {
	t.Run("snapshot on the alternate screen isolates a native attachment", func(t *testing.T) {
		models, out := startAutoNative(t, attachConfig{})
		t.Cleanup(func() {
			if err := models.finish(attachOutcomeAborted); err != nil {
				t.Errorf("finish: %v", err)
			}
		})
		mark := len(out.String())
		resized, err := models.applyOutput(sessionSnapshot(t, "shell$ vim\x1b[?1049h\x1b[2J\x1b[HEDITOR"))
		if err != nil {
			t.Fatalf("applyOutput: %v", err)
		}
		if resized || models.presentation != presentationIsolated {
			t.Fatalf("resized=%v presentation=%v, want isolated without a size change", resized, models.presentation)
		}
		// The snapshot's own screen switches never reach the terminal: only
		// the withdrawal of natively forwarded modes and the client's
		// protected entry do, followed by paints.
		if direct := withoutPaints(out.String()[mark:]); direct != nativeModeReset+isolatedEntry {
			t.Fatalf("direct output %q, want only %q", direct, nativeModeReset+isolatedEntry)
		}
		waitForPaint(t, out, func(paint string) bool { return strings.Contains(paint, "EDITOR") })
	})

	t.Run("snapshot on the primary screen returns an isolated attachment to native", func(t *testing.T) {
		models, out := newTestModels(t, attachConfig{mode: AttachModeAuto, showDetachInstructions: true})
		t.Cleanup(func() {
			if err := models.finish(attachOutcomeAborted); err != nil {
				t.Errorf("finish: %v", err)
			}
		})
		if _, err := models.applyResize(80, 24); err != nil {
			t.Fatalf("applyResize: %v", err)
		}
		if _, err := models.applyOutput(sessionSnapshot(t, "shell$ vim\x1b[?1049h\x1b[2J\x1b[HEDITOR")); err != nil {
			t.Fatalf("applyOutput: %v", err)
		}
		if models.presentation != presentationIsolated {
			t.Fatalf("presentation = %v, want isolated", models.presentation)
		}
		waitForPaint(t, out, func(paint string) bool { return strings.Contains(paint, "EDITOR") })

		mark := len(out.String())
		resized, err := models.applyOutput(sessionSnapshot(t, "shell$ vim\r\nshell$ \x1b[?2004h"))
		if err != nil {
			t.Fatalf("applyOutput: %v", err)
		}
		if !resized || models.presentation != presentationNative || models.view != nil {
			t.Fatalf("resized=%v presentation=%v view=%v, want native without a view", resized, models.presentation, models.view != nil)
		}
		direct := withoutPaints(out.String()[mark:])
		// Leaving isolation reveals the pre-attach screen, which native entry
		// scrolls into scrollback before the primary screen is repainted with
		// the modes the physical terminal missed while isolated.
		wantPrefix := "\x1b[?1049l\x1b[r\x1b[24;1H" + strings.Repeat("\r\n", 24) + "\x1b[H" + nativeTitleHint + "\x1b[2J\x1b[H\x1b[0m"
		if !strings.HasPrefix(direct, wantPrefix) {
			t.Fatalf("direct output %q, want prefix %q", direct, wantPrefix)
		}
		if !strings.Contains(direct, "\x1b[?2004h") {
			t.Fatalf("direct output %q did not restore bracketed paste", direct)
		}
		for _, forbidden := range []string{"\x1b[?1049h", "\x1bc", "\x1b[3J", "EDITOR"} {
			if strings.Contains(direct, forbidden) {
				t.Fatalf("direct output %q contains %q", direct, forbidden)
			}
		}
		if strings.Count(direct, detachHint) != 1 {
			t.Fatalf("first native presentation should show the one-time hint once: %q", direct)
		}
		rendered := renderOutput(t, out.String(), 80, 24)
		if !strings.Contains(rendered, "shell$ vim") || strings.Contains(rendered, "EDITOR") {
			t.Fatalf("rendered screen %q, want the primary screen restored", rendered)
		}
	})
}

func TestAttachModelsForcedModesNeverTransitionOnLiveOutput(t *testing.T) {
	const payload = "shell$ \x1b[?1049h\x1b[2J\x1b[HTUI\x1b[?1049lback"

	t.Run("native forwards screen switches raw", func(t *testing.T) {
		models, out := newTestModels(t, attachConfig{mode: AttachModeNative})
		if _, err := models.applyResize(80, 24); err != nil {
			t.Fatalf("applyResize: %v", err)
		}
		resized, err := models.applyOutput([]byte(payload))
		if err != nil {
			t.Fatalf("applyOutput: %v", err)
		}
		if resized || models.presentation != presentationNative || models.view != nil {
			t.Fatalf("resized=%v presentation=%v view=%v, want native untouched", resized, models.presentation, models.view != nil)
		}
		if !strings.HasSuffix(out.String(), payload) {
			t.Fatalf("output %q does not end with the raw payload", out.String())
		}
		if err := models.finish(attachOutcomeAborted); err != nil {
			t.Fatalf("finish: %v", err)
		}
	})

	t.Run("isolated keeps painting", func(t *testing.T) {
		models, out := newTestModels(t, attachConfig{mode: AttachModeIsolated})
		t.Cleanup(func() {
			if err := models.finish(attachOutcomeAborted); err != nil {
				t.Errorf("finish: %v", err)
			}
		})
		resized, err := models.applyOutput([]byte(payload))
		if err != nil {
			t.Fatalf("applyOutput: %v", err)
		}
		if resized || models.presentation != presentationIsolated || models.view == nil {
			t.Fatalf("resized=%v presentation=%v view=%v, want isolated with a view", resized, models.presentation, models.view != nil)
		}
		if direct := withoutPaints(out.String()); direct != isolatedEntry {
			t.Fatalf("direct output %q, want only %q", direct, isolatedEntry)
		}
	})
}

func TestAttachModelsCleanupFollowsPresentationAtExit(t *testing.T) {
	t.Run("detach while isolated after a native phase", func(t *testing.T) {
		models, out := startAutoNative(t, attachConfig{showDetachInstructions: true})
		if _, err := models.applyOutput([]byte("\x1b[?1049h\x1b[2J\x1b[HTUI")); err != nil {
			t.Fatalf("applyOutput: %v", err)
		}
		mark := len(out.String())
		if err := models.finish(attachOutcomeDetached); err != nil {
			t.Fatalf("finish: %v", err)
		}
		got := withoutPaints(out.String()[mark:])
		if !strings.HasSuffix(got, "\x1b[?1049l"+nativeCleanup+"\r\nDetached from session\r\n") {
			t.Fatalf("cleanup %q, want alternate screen left then native restoration", got)
		}
		if strings.Contains(got, finalAttachScreenPrefix(attachSnapshot{physicalRows: 24})) {
			t.Fatalf("cleanup %q reprinted the final screen on detach", got)
		}
		// The one-time hint drawn natively before isolating is erased from the
		// restored primary screen.
		rendered := renderOutput(t, out.String(), 80, 24)
		if strings.Contains(rendered, detachHint) || strings.Contains(rendered, "TUI") {
			t.Fatalf("rendered screen %q still shows the hint or alternate screen", rendered)
		}
		if !strings.Contains(rendered, "shell$ ") || !strings.Contains(rendered, "Detached from session") {
			t.Fatalf("rendered screen %q lacks the primary content and outcome", rendered)
		}
	})

	t.Run("detach while native after an isolated start", func(t *testing.T) {
		models, out := newTestModels(t, attachConfig{mode: AttachModeAuto})
		if _, err := models.applyResize(80, 24); err != nil {
			t.Fatalf("applyResize: %v", err)
		}
		if _, err := models.applyOutput(sessionSnapshot(t, "shell$ vim\x1b[?1049h\x1b[2J\x1b[HEDITOR")); err != nil {
			t.Fatalf("applyOutput: %v", err)
		}
		if _, err := models.applyOutput([]byte("\x1b[?1049l\r\nshell$ ")); err != nil {
			t.Fatalf("applyOutput: %v", err)
		}
		mark := len(out.String())
		if err := models.finish(attachOutcomeDetached); err != nil {
			t.Fatalf("finish: %v", err)
		}
		got := out.String()[mark:]
		if !strings.HasPrefix(got, nativeCleanup) {
			t.Fatalf("cleanup %q, want native cleanup first", got)
		}
		if strings.Contains(got, "\x1b[?1049l") {
			t.Fatalf("cleanup %q left an alternate screen the terminal is not on", got)
		}
		if !strings.HasSuffix(got, "Detached from session\r\n") {
			t.Fatalf("cleanup %q lacks the outcome line", got)
		}
		rendered := renderOutput(t, out.String(), 80, 24)
		if !strings.Contains(rendered, "shell$ vim") || strings.Contains(rendered, "EDITOR") {
			t.Fatalf("rendered screen %q, want the primary screen without the editor", rendered)
		}
	})
}
