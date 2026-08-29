package election

import "time"

// Clock exposes the two independent time readings this package needs, as
// separate methods, because the difference between them is load-bearing.
//
// Go's time.Time carries both a wall-clock reading and a monotonic one, and
// t2.Sub(t1) silently prefers the monotonic reading. That is normally what you
// want — it is immune to NTP steps — but it is exactly wrong for detecting that
// this process was suspended: the monotonic clock does not advance while the
// machine is asleep. A laptop that slept for four hours reports a few
// milliseconds of monotonic elapsed time on wake.
//
// Splitting the readings makes the discrepancy observable. When wall-clock
// elapsed time greatly exceeds monotonic elapsed time, the process lost time it
// did not experience, and any conclusion it drew before the gap is stale.
type Clock interface {
	// Wall is the current wall-clock time. It can jump, forwards or backwards,
	// on an NTP correction or a resume.
	Wall() time.Time
	// Mono is a monotonically non-decreasing duration since some fixed origin.
	// It never goes backwards and never jumps forwards — and, crucially, does
	// not advance while the machine is suspended.
	Mono() time.Duration
}

// systemClock is the real Clock.
type systemClock struct{ origin time.Time }

// SystemClock returns a Clock reading the host's actual time.
func SystemClock() Clock { return systemClock{origin: time.Now()} }

// Wall strips the monotonic reading so callers cannot accidentally get
// monotonic subtraction semantics from a value that is meant to be wall time.
func (c systemClock) Wall() time.Time { return time.Now().Round(0) }

// Mono uses time.Since against a fixed origin, which Go computes from the
// monotonic reading both values carry.
func (c systemClock) Mono() time.Duration { return time.Since(c.origin) }

// instant is a paired reading of both clocks, taken at the same moment. Storing
// the pair is what lets a later reading tell "time passed normally" from "this
// process was not running for a while".
type instant struct {
	wall time.Time
	mono time.Duration
}

// now takes a paired reading.
func now(c Clock) instant { return instant{wall: c.Wall(), mono: c.Mono()} }

// since reports how much time has passed since the earlier instant, according
// to each clock.
//
// The wall reading is clamped at zero: a backwards NTP step would otherwise
// produce a negative elapsed time and make a stale lease look fresh.
func (i instant) since(later instant) (wall, mono time.Duration) {
	wall = later.wall.Sub(i.wall)
	if wall < 0 {
		wall = 0
	}
	mono = later.mono - i.mono
	if mono < 0 {
		mono = 0
	}
	return wall, mono
}

// suspendSlack is how far wall-clock elapsed time may exceed monotonic elapsed
// time before the gap is treated as a suspension rather than as ordinary clock
// noise. Routine NTP corrections are milliseconds; a suspend is seconds at the
// very least.
const suspendSlack = 2 * time.Second
