package bgx

import (
	"errors"
	"testing"
	"time"

	"github.com/sidedotdev/bgx/vt"
)

func TestAttachViewReportsAsynchronousPaintError(t *testing.T) {
	term, err := vt.New(80, 23)
	if err != nil {
		t.Fatalf("vt.New: %v", err)
	}
	defer term.Close()

	paintErr := errors.New("paint failed")
	errs := make(chan error, 1)
	view := newAttachView(func(string) error {
		return paintErr
	}, term, errs, 23, true)

	select {
	case <-view.stopped:
	case <-time.After(time.Second):
		t.Fatal("painter did not stop after write failure")
	}
	select {
	case err := <-errs:
		if !errors.Is(err, paintErr) {
			t.Fatalf("reported error = %v, want paint failure", err)
		}
	default:
		t.Fatal("paint failure was not reported")
	}
	if err := view.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
