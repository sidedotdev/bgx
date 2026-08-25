package bgx

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

// pairedModelRecorder implements both attach models, recording the order of
// operations and optionally blocking inside one of them so tests can attempt
// to interleave a concurrent operation at that exact point.
type pairedModelRecorder struct {
	mu     sync.Mutex
	events []string

	blockOn string
	started chan struct{}
	release chan struct{}
}

func (r *pairedModelRecorder) record(event string) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
	if event == r.blockOn {
		close(r.started)
		<-r.release
	}
}

func (r *pairedModelRecorder) recordedEvents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *pairedModelRecorder) Write(p []byte) (int, error) {
	r.record("screen.write")
	return len(p), nil
}

func (r *pairedModelRecorder) Resize(uint16, uint16) error {
	r.record("screen.resize")
	return nil
}

func (r *pairedModelRecorder) DumpScreen() ([]byte, error) {
	r.record("screen.dump")
	return nil, nil
}

func (r *pairedModelRecorder) WrittenRows() (uint16, error) {
	r.record("screen.writtenRows")
	return 0, nil
}

func (r *pairedModelRecorder) feed([]byte) error {
	r.record("view.feed")
	return nil
}

func (r *pairedModelRecorder) setSize(_, physicalRows uint16) (uint16, error) {
	r.record("view.setSize")
	return physicalRows, nil
}

func TestAttachModelsResizeCannotInterleaveOutput(t *testing.T) {
	recorder := &pairedModelRecorder{
		blockOn: "screen.write",
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	models := &attachModels{screen: recorder, view: recorder}

	outputDone := make(chan error, 1)
	go func() {
		outputDone <- models.applyOutput([]byte("payload"))
	}()
	<-recorder.started

	resizeDone := make(chan error, 1)
	go func() {
		_, err := models.applyResize(80, 24)
		resizeDone <- err
	}()

	select {
	case <-resizeDone:
		t.Fatal("resize completed while an output payload was mid-application")
	case <-time.After(50 * time.Millisecond):
	}

	close(recorder.release)
	if err := <-outputDone; err != nil {
		t.Fatalf("applyOutput: %v", err)
	}
	if err := <-resizeDone; err != nil {
		t.Fatalf("applyResize: %v", err)
	}

	want := []string{"screen.write", "view.feed", "view.setSize", "screen.resize"}
	if got := recorder.recordedEvents(); !reflect.DeepEqual(got, want) {
		t.Fatalf("event order = %v, want %v", got, want)
	}
}

func TestAttachModelsOutputCannotInterleaveResize(t *testing.T) {
	recorder := &pairedModelRecorder{
		blockOn: "view.setSize",
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	models := &attachModels{screen: recorder, view: recorder}

	resizeDone := make(chan error, 1)
	go func() {
		_, err := models.applyResize(80, 24)
		resizeDone <- err
	}()
	<-recorder.started

	outputDone := make(chan error, 1)
	go func() {
		outputDone <- models.applyOutput([]byte("payload"))
	}()

	select {
	case <-outputDone:
		t.Fatal("output completed while a resize was mid-application")
	case <-time.After(50 * time.Millisecond):
	}

	close(recorder.release)
	if err := <-resizeDone; err != nil {
		t.Fatalf("applyResize: %v", err)
	}
	if err := <-outputDone; err != nil {
		t.Fatalf("applyOutput: %v", err)
	}

	want := []string{"view.setSize", "screen.resize", "screen.write", "view.feed"}
	if got := recorder.recordedEvents(); !reflect.DeepEqual(got, want) {
		t.Fatalf("event order = %v, want %v", got, want)
	}
}
