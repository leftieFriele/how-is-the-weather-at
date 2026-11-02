// Package forecast caches met.no forecasts per location and decides when met.no may be called.
//
// It enforces the freshness rule (data is never served more than 2 hours after met.no last confirmed it), the
// met.no Terms of Service (never call before Expires, stay within the upstream budget, back off on errors) and
// makes concurrent requests for the same location share one met.no call.
package forecast

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/simplelru"

	"how-is-the-weather-at-api/internal/geo"
	"how-is-the-weather-at-api/internal/metno"
)

const (
	// Measured from when met.no last confirmed the data.
	MaxAge = 2 * time.Hour
	// How long clients may cache data while a refresh is due.
	RefreshingFreshFor = 60 * time.Second

	upstreamTimeout   = 10 * time.Second
	waitTimeout       = 3 * time.Second
	refreshJitter     = 60 * time.Second
	backoffMin        = 30 * time.Second
	backoffMax        = 5 * time.Minute
	deprecatedLogEach = time.Hour
	budgetRetryAfter  = time.Second
)

var (
	errUpstreamTimeout = errors.New("met.no did not answer within 10s")
	errWaitTimeout     = errors.New("gave up waiting for met.no after 3s")
)

type Fetcher interface {
	Get(ctx context.Context, p geo.Point, lastModified string) (metno.Forecast, error)
}

type Data struct {
	// Ordered by time. Shared, so must not be modified.
	Steps []metno.Step
	// The last 200 or 304 from met.no.
	ValidatedAt time.Time
	// ValidatedAt + MaxAge. The data is never served after this.
	Deadline time.Time
	// The time until met.no's Expires, or RefreshingFreshFor once it has passed. Capping it at Deadline is up to the caller.
	FreshFor time.Duration
}

// UnavailableError means there is no data younger than 2 hours, and it can't be fetched right now.
type UnavailableError struct {
	Status     int           // http.StatusBadGateway or http.StatusServiceUnavailable
	RetryAfter time.Duration // 0 if unknown
	Reason     string
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("forecast unavailable (%d): %s", e.Status, e.Reason)
}

func unavailable(status int, retryAfter time.Duration, reason string) *UnavailableError {
	return &UnavailableError{Status: status, RetryAfter: retryAfter, Reason: reason}
}

type Config struct {
	Fetcher Fetcher
	Budget  *Budget
	Size    int
	Logger  *slog.Logger
}

type Cache struct {
	fetcher Fetcher
	budget  *Budget
	log     *slog.Logger
	ctx     context.Context // server lifetime; met.no calls run on it, not on the request's context
	wg      sync.WaitGroup

	mu                 sync.Mutex
	entries            *simplelru.LRU[geo.Point, *entry]
	inflight           map[geo.Point]*call
	gate               gate
	deprecatedLoggedAt time.Time
}

type entry struct {
	steps        []metno.Step // nil until the first successful fetch
	validatedAt  time.Time
	expires      time.Time
	refreshAfter time.Time // expires plus jitter, so refreshes are spread out
	lastModified string
	backoffUntil time.Time
	backoffNext  time.Duration
}

// done is closed once data or err is set.
type call struct {
	done chan struct{}
	data Data
	err  error
}

// Cancelling ctx cancels all met.no calls; Wait then waits for them to end.
func New(ctx context.Context, cfg Config) (*Cache, error) {
	entries, err := simplelru.NewLRU[geo.Point, *entry](cfg.Size, nil)
	if err != nil {
		return nil, fmt.Errorf("forecast: %w", err)
	}
	return &Cache{
		fetcher:  cfg.Fetcher,
		budget:   cfg.Budget,
		log:      cfg.Logger,
		ctx:      ctx,
		entries:  entries,
		inflight: make(map[geo.Point]*call),
	}, nil
}

// Cancel the context given to New first.
func (c *Cache) Wait() { c.wg.Wait() }

// Get returns the forecast for p. If there is none younger than 2 hours, it fetches one and waits for it, at most
// 3 seconds. Errors are *UnavailableError.
func (c *Cache) Get(ctx context.Context, p geo.Point) (Data, error) {
	c.mu.Lock()
	now := time.Now()
	e, _ := c.entries.Get(p)
	if e.usable(now) {
		if !now.Before(e.refreshAfter) && !now.Before(e.backoffUntil) {
			// Refresh in the background; this request is served from the cache either way.
			if _, err := c.startLocked(now, p, e); err != nil {
				c.log.Debug("background refresh skipped", locationAttr(p), slog.Any("reason", err))
			}
		}
		d := e.data(now)
		c.mu.Unlock()
		return d, nil
	}
	cl, err := c.fetchLocked(now, p, e)
	c.mu.Unlock()
	if err != nil {
		return Data{}, err
	}
	return wait(ctx, cl)
}

// c.mu must be held.
func (c *Cache) fetchLocked(now time.Time, p geo.Point, e *entry) (*call, error) {
	if cl := c.inflight[p]; cl != nil {
		return cl, nil
	}
	if e != nil && now.Before(e.backoffUntil) {
		return nil, unavailable(http.StatusBadGateway, e.backoffUntil.Sub(now), "met.no failed for this location recently")
	}
	if e.hasData() && now.Before(e.expires) {
		// The data is past its 2-hour deadline, but the Terms of Service forbid calling met.no before Expires.
		c.log.Warn("location unavailable: met.no Expires is later than the 2-hour deadline",
			locationAttr(p), slog.Time("expires", e.expires))
		return nil, unavailable(http.StatusServiceUnavailable, e.expires.Sub(now), "met.no's Expires has not passed")
	}
	return c.startLocked(now, p, e)
}

// Joins a call already running for p. Fails if calls are paused or the budget has no token. c.mu must be held.
func (c *Cache) startLocked(now time.Time, p geo.Point, e *entry) (*call, error) {
	if cl := c.inflight[p]; cl != nil {
		return cl, nil
	}
	if ok, retryAfter := c.gate.allow(now); !ok {
		return nil, unavailable(http.StatusServiceUnavailable, retryAfter, "calls to met.no are paused")
	}
	known := e.hasData()
	if !c.budget.TryTake(known) {
		return nil, unavailable(http.StatusServiceUnavailable, budgetRetryAfter, "no upstream budget left")
	}
	lastModified := ""
	if known {
		lastModified = e.lastModified
	}
	cl := &call{done: make(chan struct{})}
	c.inflight[p] = cl
	c.wg.Go(func() { c.run(p, lastModified, cl) })
	return cl, nil
}

// If the wait times out first, the call keeps running and fills the cache.
func wait(ctx context.Context, cl *call) (Data, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, waitTimeout, errWaitTimeout)
	defer cancel()
	select {
	case <-cl.done:
		return cl.data, cl.err
	case <-ctx.Done():
		return Data{}, unavailable(http.StatusServiceUnavailable, time.Second, context.Cause(ctx).Error())
	}
}

func (c *Cache) run(p geo.Point, lastModified string, cl *call) {
	ctx, cancel := context.WithTimeoutCause(c.ctx, upstreamTimeout, errUpstreamTimeout)
	defer cancel()
	f, err := c.fetcher.Get(ctx, p, lastModified)

	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	e, ok := c.entries.Get(p)
	if !ok {
		e = &entry{}
		c.entries.Add(p, e)
	}
	cl.data, cl.err = c.storeLocked(now, p, e, f, err)
	delete(c.inflight, p)
	close(cl.done)
}

// Returns what waiting requests get. c.mu must be held.
func (c *Cache) storeLocked(now time.Time, p geo.Point, e *entry, f metno.Forecast, err error) (Data, error) {
	if err == nil && f.NotModified && !e.hasData() {
		// The entry was evicted while the call ran, so there is nothing to keep.
		err = &metno.UpstreamError{Status: http.StatusNotModified, Err: errors.New("not modified, but nothing cached")}
	}
	c.gate.report(now, err)

	if err == nil {
		if !f.NotModified {
			e.steps = f.Steps
		}
		e.validatedAt = now
		e.expires = f.Expires
		e.refreshAfter = f.Expires.Add(rand.N(refreshJitter))
		e.lastModified = f.LastModified
		e.backoffUntil, e.backoffNext = time.Time{}, 0
		if f.Deprecated && now.Sub(c.deprecatedLoggedAt) >= deprecatedLogEach {
			c.deprecatedLoggedAt = now
			c.log.Warn("met.no says this product version is deprecated (203); migrate to a newer version")
		}
		return e.data(now), nil
	}

	var fail *UnavailableError
	if _, ok := errors.AsType[*metno.ThrottledError](err); ok {
		c.log.Warn("met.no throttled us (429); pausing all calls", slog.Any("error", err))
		fail = unavailable(http.StatusServiceUnavailable, c.gate.wait(now), "met.no asked us to back off")
	} else if _, ok := errors.AsType[*metno.ForbiddenError](err); ok {
		c.log.Error("met.no refused our request (403); pausing all calls, a human must check this", slog.Any("error", err))
		fail = unavailable(http.StatusServiceUnavailable, c.gate.wait(now), "calls to met.no are paused")
	} else if _, ok := errors.AsType[*metno.ClientError](err); ok {
		c.log.Error("met.no rejected our request", locationAttr(p), slog.Any("error", err))
		e.backOff(now, backoffMax)
		fail = unavailable(http.StatusBadGateway, e.backoffUntil.Sub(now), "met.no rejected the request")
	} else {
		c.log.Warn("met.no call failed", locationAttr(p), slog.Any("error", err))
		d := max(e.backoffNext, backoffMin)
		e.backoffNext = min(2*d, backoffMax)
		e.backOff(now, min(d+rand.N(d/5), backoffMax))
		fail = unavailable(http.StatusBadGateway, e.backoffUntil.Sub(now), "met.no failed")
	}

	// Data younger than 2 hours is still served.
	if e.usable(now) {
		return e.data(now), nil
	}
	return Data{}, fail
}

func (e *entry) hasData() bool { return e != nil && e.steps != nil }

func (e *entry) deadline() time.Time { return e.validatedAt.Add(MaxAge) }

func (e *entry) usable(now time.Time) bool { return e.hasData() && now.Before(e.deadline()) }

func (e *entry) data(now time.Time) Data {
	freshFor := RefreshingFreshFor
	if now.Before(e.expires) {
		freshFor = e.expires.Sub(now)
	}
	return Data{Steps: e.steps, ValidatedAt: e.validatedAt, Deadline: e.deadline(), FreshFor: freshFor}
}

// While the entry still has data that may be served, the backoff ends halfway to the deadline at the latest,
// so at least one retry happens before it.
func (e *entry) backOff(now time.Time, d time.Duration) {
	if e.usable(now) {
		d = min(d, e.deadline().Sub(now)/2)
	}
	e.backoffUntil = now.Add(d)
}

func locationAttr(p geo.Point) slog.Attr {
	return slog.Group("location", slog.String("lat", p.Lat.String()), slog.String("lon", p.Lon.String()))
}
