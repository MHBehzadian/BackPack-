// Package health turns a stream of probe results into a server status.
package health

import (
	"time"
)

type Status int

const (
	Unknown  Status = iota // not enough probes yet
	Up                     // answering
	Unstable               // answering, but dropped FlapMaxDrops times inside the flap window
	Down                   // FailThreshold failures in a row
)

func (s Status) String() string {
	switch s {
	case Up:
		return "up"
	case Unstable:
		return "unstable"
	case Down:
		return "down"
	}
	return "unknown"
}

// Usable reports whether traffic may be sent to a server in this status.
func (s Status) Usable() bool { return s == Up }

type Params struct {
	FailThreshold    int
	RecoverThreshold int
	FlapWindow       time.Duration
	FlapMaxDrops     int
}

// Tracker keeps the recent history of one server. It is not safe for
// concurrent use; the controller owns it.
type Tracker struct {
	p      Params
	status Status
	drops  []time.Time // start of every failure run inside the flap window
	lastOK bool

	consecFail int
	consecOK   int

	started     time.Time
	lastFailure time.Time
	LastRTT     time.Duration
	LastErr     error
	LastCheck   time.Time
	Total       int
	Failed      int
}

func NewTracker(p Params, now time.Time) *Tracker {
	return &Tracker{p: p, started: now}
}

func (t *Tracker) Status() Status { return t.status }

// DropsInWindow is how many times the server went from answering to failing
// inside the flap window. A long outage is one drop; flapping is many.
func (t *Tracker) DropsInWindow() int { return len(t.drops) }

// LastFailure is the time of the most recent failed probe (zero if none).
func (t *Tracker) LastFailure() time.Time { return t.lastFailure }

// CleanFor is how long the server has answered every probe, counted from the
// last failure or from when monitoring started.
func (t *Tracker) CleanFor(now time.Time) time.Duration {
	if t.status != Up {
		return 0
	}
	from := t.started
	if t.lastFailure.After(from) {
		from = t.lastFailure
	}
	return now.Sub(from)
}

// Record adds a probe result and returns the status before and after it.
func (t *Tracker) Record(now time.Time, ok bool, rtt time.Duration, err error) (old, cur Status) {
	old = t.status
	t.LastCheck = now
	t.Total++
	cutoff := now.Add(-t.p.FlapWindow)
	i := 0
	for i < len(t.drops) && !t.drops[i].After(cutoff) {
		i++
	}
	t.drops = t.drops[i:]

	if ok {
		t.consecOK++
		t.consecFail = 0
		t.LastRTT = rtt
		t.LastErr = nil
	} else {
		t.Failed++
		t.consecFail++
		t.consecOK = 0
		t.lastFailure = now
		t.LastErr = err
		if t.lastOK || t.Total == 1 {
			t.drops = append(t.drops, now)
		}
	}
	t.lastOK = ok
	t.status = t.next()
	return old, t.status
}

func (t *Tracker) next() Status {
	flapping := len(t.drops) >= t.p.FlapMaxDrops
	switch {
	case t.consecFail >= t.p.FailThreshold:
		return Down
	case t.status == Down || t.status == Unknown:
		// Coming back (or starting up) needs a run of good probes.
		if t.consecOK >= t.p.RecoverThreshold {
			if flapping {
				return Unstable
			}
			return Up
		}
		if t.status == Unknown && t.consecFail > 0 && t.consecFail < t.p.FailThreshold {
			return Unknown
		}
		return t.status
	case flapping:
		return Unstable
	case t.status == Unstable:
		// Leaving "unstable" needs the window to drain below the limit and a run of good probes.
		if t.consecOK >= t.p.RecoverThreshold {
			return Up
		}
		return Unstable
	}
	return Up
}
