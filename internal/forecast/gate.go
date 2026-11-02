package forecast

import (
	"context"
	"errors"
	"time"

	"how-is-the-weather-at-api/internal/metno"
)

const (
	forbiddenPauseMin = 15 * time.Minute
	forbiddenPauseMax = time.Hour
	breakerFailures   = 5
	breakerPauseMin   = 30 * time.Second
	breakerPauseMax   = 5 * time.Minute
	probeRetryAfter   = time.Second
)

// gate decides whether met.no may be called at all. It pauses all calls after a 429 (for Retry-After), after a 403
// (15 min, then one probe, doubling up to 1h) and after 5 failures in a row (30s, doubling up to 5 min).
// It is not safe for concurrent use; the cache guards it with its mutex.
type gate struct {
	pausedUntil time.Time

	forbidden      bool // a 403 was the last word from met.no; the next call is a probe
	forbiddenPause time.Duration
	probing        bool

	failures     int // in a row, across all locations
	breakerPause time.Duration
}

// After a 403 pause, exactly one probe is let through until its result is reported.
func (g *gate) allow(now time.Time) (bool, time.Duration) {
	if wait := g.wait(now); wait > 0 {
		return false, wait
	}
	if g.forbidden {
		g.probing = true
	}
	return true, 0
}

func (g *gate) wait(now time.Time) time.Duration {
	if now.Before(g.pausedUntil) {
		return g.pausedUntil.Sub(now)
	}
	if g.probing {
		return probeRetryAfter
	}
	return 0
}

func (g *gate) report(now time.Time, err error) {
	g.probing = false
	if err == nil {
		g.failures, g.breakerPause = 0, 0
		g.forbidden, g.forbiddenPause = false, 0
		return
	}
	if e, ok := errors.AsType[*metno.ThrottledError](err); ok {
		g.pause(now, e.RetryAfter)
		return
	}
	if _, ok := errors.AsType[*metno.ForbiddenError](err); ok {
		g.forbidden = true
		g.forbiddenPause = nextPause(g.forbiddenPause, forbiddenPauseMin, forbiddenPauseMax)
		g.pause(now, g.forbiddenPause)
		return
	}
	// A cancelled call during shutdown says nothing about met.no. Neither does another 4xx: met.no answered.
	if _, ok := errors.AsType[*metno.UpstreamError](err); ok && !errors.Is(err, context.Canceled) {
		g.failures++
		if g.failures >= breakerFailures {
			g.breakerPause = nextPause(g.breakerPause, breakerPauseMin, breakerPauseMax)
			g.pause(now, g.breakerPause)
		}
	}
}

func (g *gate) pause(now time.Time, d time.Duration) {
	if until := now.Add(d); until.After(g.pausedUntil) {
		g.pausedUntil = until
	}
}

func nextPause(current, lo, hi time.Duration) time.Duration {
	if current == 0 {
		return lo
	}
	return min(2*current, hi)
}
