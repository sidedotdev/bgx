package bgx

import (
	"errors"
	"testing"
	"time"
)

func TestAttachViewCloseRetainsAsynchronousPaintError(t *testing.T) {
	paintErr := errors.New("paint failed")
	view, err := newAttachView(func(string) error {
		return paintErr
	}, 80, 24, true)
	if err != nil {
		t.Fatalf("newAttachView: %v", err)
	}

	select {
	case <-view.stopped:
	case <-time.After(time.Second):
		t.Fatal("painter did not stop after write failure")
	}

	if err := view.close(); !errors.Is(err, paintErr) {
		t.Fatalf("close error = %v, want paint failure", err)
	}
}
