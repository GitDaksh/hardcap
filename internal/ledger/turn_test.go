package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

// One budget write per project at a time; other projects aren't held up,
// and a caller that gives up stops waiting.
func TestTurnsQueuePerProject(t *testing.T) {
	var l Ledger
	ctx := context.Background()

	done, err := l.turn(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}

	otherProject, err := l.turn(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	otherProject()

	waited := make(chan struct{})
	go func() {
		next, err := l.turn(ctx, 1)
		if err != nil {
			t.Error(err)
		}
		next()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("a second turn on project 1 started before the first ended")
	case <-time.After(50 * time.Millisecond):
	}

	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := l.turn(short, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting past the deadline: got %v, want context.DeadlineExceeded", err)
	}

	done()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("the waiting turn never started")
	}
}
