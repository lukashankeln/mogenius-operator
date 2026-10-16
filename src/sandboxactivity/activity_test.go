package sandboxactivity

import (
	"testing"
	"time"
)

func TestTouchHoldAndPrune(t *testing.T) {
	clock := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	tr := NewTracker(func() time.Time { return clock })

	if at, open := tr.Last("ns", "sb"); !at.IsZero() || open {
		t.Fatalf("unseen pod = %v, %v", at, open)
	}
	tr.Touch("ns", "sb")
	if at, _ := tr.Last("ns", "sb"); !at.Equal(clock) {
		t.Fatalf("touch = %v", at)
	}

	release := tr.Hold("ns", "sb")
	second := tr.Hold("ns", "sb")
	clock = clock.Add(time.Hour)
	release()
	release() // a second call changes nothing
	if _, open := tr.Last("ns", "sb"); !open {
		t.Fatal("one of two connections is still open")
	}
	second()
	if at, open := tr.Last("ns", "sb"); open || !at.Equal(clock) {
		t.Fatalf("after both ended = %v, %v; the end counts as activity", at, open)
	}

	tr.Touch("ns", "other")
	stays := tr.Hold("ns", "streaming")
	tr.Prune(func(_, pod string) bool { return pod == "sb" })
	if at, _ := tr.Last("ns", "other"); !at.IsZero() {
		t.Fatal("a pod that is no sandbox was kept")
	}
	if _, open := tr.Last("ns", "streaming"); !open {
		t.Fatal("prune dropped an open connection")
	}
	stays()
	tr.Touch("", "x")
	tr.Hold("ns", "")()
	if len(tr.last) != 2 {
		t.Fatalf("empty names were tracked: %v", tr.last)
	}
}
