package health

import (
	"errors"
	"testing"
	"time"
)

var p = Params{FailThreshold: 3, RecoverThreshold: 3, FlapWindow: 5 * time.Minute, FlapMaxDrops: 3}

func feed(t *Tracker, now *time.Time, pattern string) Status {
	for _, ch := range pattern {
		*now = now.Add(10 * time.Second)
		t.Record(*now, ch == '+', time.Millisecond, errors.New("x"))
	}
	return t.Status()
}

func TestStartupNeedsRecoverRun(t *testing.T) {
	now := time.Unix(0, 0)
	tr := NewTracker(p, now)
	if s := feed(tr, &now, "++"); s != Unknown {
		t.Fatalf("after 2 ok: %s", s)
	}
	if s := feed(tr, &now, "+"); s != Up {
		t.Fatalf("after 3 ok: %s", s)
	}
}

func TestSingleBlipIsIgnored(t *testing.T) {
	now := time.Unix(0, 0)
	tr := NewTracker(p, now)
	feed(tr, &now, "+++")
	if s := feed(tr, &now, "-+++-++"); s != Up {
		t.Fatalf("blips: %s", s)
	}
}

func TestDownAndRecover(t *testing.T) {
	now := time.Unix(0, 0)
	tr := NewTracker(p, now)
	feed(tr, &now, "+++")
	if s := feed(tr, &now, "---"); s != Down {
		t.Fatalf("3 fails: %s", s)
	}
	if s := feed(tr, &now, "++"); s != Down {
		t.Fatalf("2 ok after down: %s", s)
	}
	// One long outage is a single drop, not flapping.
	if s := feed(tr, &now, "+"); s != Up {
		t.Fatalf("3 ok after down: %s", s)
	}
}

func TestFlappingIsUnstable(t *testing.T) {
	now := time.Unix(0, 0)
	tr := NewTracker(p, now)
	feed(tr, &now, "+++")
	if s := feed(tr, &now, "-+-+-+"); s != Unstable {
		t.Fatalf("flapping: %s", s)
	}
	// Window must drain: 5 minutes of good probes brings it back to Up.
	if s := feed(tr, &now, "++++++++++++++++++++++++++++++++"); s != Up {
		t.Fatalf("after drain: %s", s)
	}
}

func TestCleanFor(t *testing.T) {
	now := time.Unix(0, 0)
	tr := NewTracker(p, now)
	feed(tr, &now, "+++--+++")
	failAt := tr.LastFailure()
	feed(tr, &now, "++++++")
	if got, want := tr.CleanFor(now), now.Sub(failAt); got != want {
		t.Fatalf("CleanFor = %s, want %s", got, want)
	}
	feed(tr, &now, "---")
	if tr.CleanFor(now) != 0 {
		t.Fatal("CleanFor must be 0 while down")
	}
}
