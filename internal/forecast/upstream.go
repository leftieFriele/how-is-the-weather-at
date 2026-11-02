package forecast

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"how-is-the-weather-at-api/internal/geo"
	"how-is-the-weather-at-api/internal/metno"
)

// The rules for when met.no may be called. They live in the Store, so they hold across all instances: pauses after
// a 429 (for Retry-After), after a 403 (15 min, then one probe, doubling up to 1h) and after 5 failures in a row
// (30s, doubling up to 5 min); one call per location at a time; per-location backoff; the upstream budget.

const (
	keyPause     = "pause"     // until when all calls are paused, in Unix ms
	keyForbidden = "forbidden" // the current 403 pause, in ms; while set, the next call is a probe
	keyProbe     = "probe"
	keyFailures  = "failures"
	keyBreaker   = "breaker" // the current failure pause, in ms

	// Outlives the 10s met.no timeout, so a lease left by a crashed instance expires.
	leaseTTL = 15 * time.Second

	forbiddenPauseMin = 15 * time.Minute
	forbiddenPauseMax = time.Hour
	breakerFailures   = 5
	breakerPauseMin   = 30 * time.Second
	breakerPauseMax   = 5 * time.Minute
	probeRetryAfter   = time.Second
	// How long the store keeps counters and pause lengths nobody updates.
	stateTTL = time.Hour
)

// errLeaseHeld means another call, possibly on another instance, is fetching the location.
var errLeaseHeld = errors.New("another call is fetching this location")

func keyForecast(p geo.Point) string { return "forecast:" + p.Lat.String() + ":" + p.Lon.String() }
func keyLease(p geo.Point) string    { return "lease:" + p.Lat.String() + ":" + p.Lon.String() }
func keyBackoff(p geo.Point) string  { return "backoff:" + p.Lat.String() + ":" + p.Lon.String() }

type permit struct {
	probe bool
}

type backoff struct {
	until time.Time
	next  time.Duration
}

// acquire takes the lease for calling met.no for p, if no pause or backoff stops it. Before the call, spend must
// take a budget token. Errors are *UnavailableError, errLeaseHeld or store errors.
func (c *Cache) acquire(ctx context.Context, p geo.Point) (permit, error) {
	now := time.Now()
	pausedUntil, err := c.getTime(ctx, keyPause)
	if err != nil {
		return permit{}, err
	}
	if now.Before(pausedUntil) {
		return permit{}, unavailable(http.StatusServiceUnavailable, pausedUntil.Sub(now), "calls to met.no are paused")
	}
	b, err := c.getBackoff(ctx, p)
	if err != nil {
		return permit{}, err
	}
	if now.Before(b.until) {
		return permit{}, unavailable(http.StatusBadGateway, b.until.Sub(now), "met.no failed for this location recently")
	}
	_, forbidden, err := c.store.get(ctx, keyForbidden)
	if err != nil {
		return permit{}, err
	}
	if forbidden {
		probeUntil, err := c.getTime(ctx, keyProbe)
		if err != nil {
			return permit{}, err
		}
		if now.Before(probeUntil) {
			return permit{}, unavailable(http.StatusServiceUnavailable, probeRetryAfter, "calls to met.no are paused")
		}
	}
	if ok, err := c.store.lease(ctx, keyLease(p), now, now.Add(leaseTTL)); err != nil || !ok {
		return permit{}, cmp.Or[error](err, errLeaseHeld)
	}
	return permit{probe: forbidden}, nil
}

// spend takes a budget token for the call, and after a 403 pause makes it the probe: no other call is let through
// until its result is reported. On failure the lease is released.
func (c *Cache) spend(ctx context.Context, p geo.Point, pm permit, known bool) error {
	now := time.Now()
	if ok, err := c.store.takeToken(ctx, now, known); err != nil || !ok {
		c.release(ctx, p)
		return cmp.Or[error](err, unavailable(http.StatusServiceUnavailable, budgetRetryAfter, "no upstream budget left"))
	}
	if pm.probe {
		if ok, err := c.store.lease(ctx, keyProbe, now, now.Add(leaseTTL)); err != nil || !ok {
			c.release(ctx, p)
			return cmp.Or[error](err, unavailable(http.StatusServiceUnavailable, probeRetryAfter, "calls to met.no are paused"))
		}
		// A probe that failed meanwhile set a new pause before it released the probe lease.
		pausedUntil, err := c.getTime(ctx, keyPause)
		if err != nil || now.Before(pausedUntil) {
			_ = c.store.del(ctx, keyLease(p), keyProbe)
			return cmp.Or[error](err, unavailable(http.StatusServiceUnavailable, pausedUntil.Sub(now), "calls to met.no are paused"))
		}
	}
	return nil
}

func (c *Cache) release(ctx context.Context, p geo.Point) { _ = c.store.del(ctx, keyLease(p)) }

// report records the result of a call made with permit, and returns how long to wait before trying again.
// maxBackoff caps a per-location backoff, so a location with servable data is retried before its deadline.
func (c *Cache) report(ctx context.Context, p geo.Point, pm permit, err error, maxBackoff time.Duration) (time.Duration, error) {
	now := time.Now()
	defer func() {
		keys := []string{keyLease(p)}
		if pm.probe {
			keys = append(keys, keyProbe)
		}
		_ = c.store.del(ctx, keys...)
	}()

	if err == nil {
		return 0, c.store.del(ctx, keyForbidden, keyBreaker, keyFailures, keyBackoff(p))
	}
	if e, ok := errors.AsType[*metno.ThrottledError](err); ok {
		return e.RetryAfter, c.pause(ctx, now, e.RetryAfter)
	}
	if _, ok := errors.AsType[*metno.ForbiddenError](err); ok {
		current, serr := c.getDuration(ctx, keyForbidden)
		if serr != nil {
			return 0, serr
		}
		d := nextPause(current, forbiddenPauseMin, forbiddenPauseMax)
		if serr := c.store.set(ctx, keyForbidden, strconv.FormatInt(d.Milliseconds(), 10), 0); serr != nil {
			return 0, serr
		}
		return d, c.pause(ctx, now, d)
	}
	if _, ok := errors.AsType[*metno.ClientError](err); ok {
		d := min(backoffMax, maxBackoff)
		return d, c.setBackoff(ctx, p, backoff{until: now.Add(d)})
	}
	// A cancelled call during shutdown says nothing about met.no.
	if errors.Is(err, context.Canceled) {
		return 0, nil
	}

	failures, serr := c.store.incr(ctx, keyFailures, stateTTL)
	if serr != nil {
		return 0, serr
	}
	if failures >= breakerFailures {
		current, serr := c.getDuration(ctx, keyBreaker)
		if serr != nil {
			return 0, serr
		}
		d := nextPause(current, breakerPauseMin, breakerPauseMax)
		if serr := c.store.set(ctx, keyBreaker, strconv.FormatInt(d.Milliseconds(), 10), stateTTL); serr != nil {
			return 0, serr
		}
		if serr := c.pause(ctx, now, d); serr != nil {
			return 0, serr
		}
	}
	b, serr := c.getBackoff(ctx, p)
	if serr != nil {
		return 0, serr
	}
	d := max(b.next, backoffMin)
	wait := min(d+rand.N(d/5), backoffMax, maxBackoff)
	return wait, c.setBackoff(ctx, p, backoff{until: now.Add(wait), next: min(2*d, backoffMax)})
}

func (c *Cache) pause(ctx context.Context, now time.Time, d time.Duration) error {
	return c.store.setMax(ctx, keyPause, now.Add(d).UnixMilli(), d+time.Minute)
}

func nextPause(current, lo, hi time.Duration) time.Duration {
	if current == 0 {
		return lo
	}
	return min(2*current, hi)
}

// once reports true for the first caller per name and period, across all instances.
func (c *Cache) once(ctx context.Context, name string, period time.Duration) bool {
	now := time.Now()
	ok, err := c.store.lease(ctx, "once:"+name, now, now.Add(period))
	return err == nil && ok
}

func (c *Cache) getTime(ctx context.Context, key string) (time.Time, error) {
	v, ok, err := c.store.get(ctx, key)
	if err != nil || !ok {
		return time.Time{}, err
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("forecast: invalid time in %s: %w", key, err)
	}
	return time.UnixMilli(ms), nil
}

func (c *Cache) getDuration(ctx context.Context, key string) (time.Duration, error) {
	v, ok, err := c.store.get(ctx, key)
	if err != nil || !ok {
		return 0, err
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("forecast: invalid duration in %s: %w", key, err)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func (c *Cache) getBackoff(ctx context.Context, p geo.Point) (backoff, error) {
	v, ok, err := c.store.get(ctx, keyBackoff(p))
	if err != nil || !ok {
		return backoff{}, err
	}
	until, next, _ := strings.Cut(v, ":")
	untilMs, err1 := strconv.ParseInt(until, 10, 64)
	nextMs, err2 := strconv.ParseInt(next, 10, 64)
	if err := errors.Join(err1, err2); err != nil {
		return backoff{}, fmt.Errorf("forecast: invalid backoff for %v: %w", p, err)
	}
	return backoff{until: time.UnixMilli(untilMs), next: time.Duration(nextMs) * time.Millisecond}, nil
}

func (c *Cache) setBackoff(ctx context.Context, p geo.Point, b backoff) error {
	v := strconv.FormatInt(b.until.UnixMilli(), 10) + ":" + strconv.FormatInt(b.next.Milliseconds(), 10)
	return c.store.set(ctx, keyBackoff(p), v, backoffMax+time.Minute)
}
