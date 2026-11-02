// Package forecast caches met.no forecasts per location and decides when met.no may be called.
//
// It enforces the freshness rule (data is never served more than 2 hours after met.no last confirmed it), the
// met.no Terms of Service (never call before Expires, stay within the upstream budget, back off on errors) and
// makes concurrent requests for the same location share one met.no call. With a RedisStore these rules hold across
// all instances.
package forecast

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"how-is-the-weather-at-api/internal/geo"
	"how-is-the-weather-at-api/internal/metno"
)

const (
	// Measured from when met.no last confirmed the data.
	MaxAge = 2 * time.Hour
	// How long clients may cache data while a refresh is due.
	RefreshingFreshFor = 60 * time.Second

	upstreamTimeout = 10 * time.Second
	waitTimeout     = 3 * time.Second
	// How often a request checks whether another instance has fetched the location it waits for.
	pollInterval  = 100 * time.Millisecond
	refreshJitter = 60 * time.Second
	// Guards against an Expires that is already past, e.g. through clock skew, which would otherwise make every
	// request refresh unchanged data.
	minRefreshInterval = time.Minute
	backoffMin         = 30 * time.Second
	backoffMax         = 5 * time.Minute
	deprecatedLogEach  = time.Hour
	budgetRetryAfter   = time.Second
	storeRetryAfter    = 5 * time.Second
	// For recording a call's result after the call itself has used up its context.
	storeTimeout = time.Second
)

var (
	errUpstreamTimeout = errors.New("met.no did not answer within 10s")
	errWaitTimeout     = errors.New("gave up waiting for met.no after 3s")
)

type Fetcher interface {
	Get(ctx context.Context, p geo.Point, lastModified string) (metno.Forecast, error)
}

type Data struct {
	// Ordered by time.
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
	Store   Store
	Logger  *slog.Logger
}

type Cache struct {
	fetcher   Fetcher
	store     Store
	log       *slog.Logger
	ctx       context.Context // server lifetime; met.no calls run on it, not on the request's context
	wg        sync.WaitGroup
	storeDown atomic.Bool

	mu sync.Mutex
	// Calls in progress in this instance. Across instances, a lease in the store does the same.
	inflight map[geo.Point]*call
}

// entry is how a forecast is kept in the store.
type entry struct {
	Steps        []metno.Step
	ValidatedAt  time.Time
	Expires      time.Time
	RefreshAfter time.Time // Expires plus jitter, so refreshes are spread out
	LastModified string
}

// done is closed once data or err is set.
type call struct {
	done chan struct{}
	data Data
	err  error
}

// Cancelling ctx cancels all met.no calls; Wait then waits for them to end.
func New(ctx context.Context, cfg Config) *Cache {
	return &Cache{
		fetcher:  cfg.Fetcher,
		store:    cfg.Store,
		log:      cfg.Logger,
		ctx:      ctx,
		inflight: make(map[geo.Point]*call),
	}
}

// Cancel the context given to New first.
func (c *Cache) Wait() { c.wg.Wait() }

// Ready fails when the store can't be reached, since then no forecast can be served.
func (c *Cache) Ready(ctx context.Context) error { return c.store.ping(ctx) }

// Get returns the forecast for p. If there is none younger than 2 hours, it fetches one and waits for it, at most
// 3 seconds. Errors are *UnavailableError.
func (c *Cache) Get(ctx context.Context, p geo.Point) (Data, error) {
	now := time.Now()
	e, err := c.load(ctx, p)
	if err != nil {
		return Data{}, c.storeUnavailable(err)
	}
	if e.usable(now) {
		if !now.Before(e.RefreshAfter) {
			c.start(p, e) // refresh in the background; this request is served from the cache either way
		}
		return e.data(now), nil
	}
	return wait(ctx, c.start(p, e))
}

// start returns this instance's call for p, starting one if none is running.
func (c *Cache) start(p geo.Point, prev *entry) *call {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl := c.inflight[p]; cl != nil {
		return cl
	}
	cl := &call{done: make(chan struct{})}
	c.inflight[p] = cl
	c.wg.Go(func() {
		data, err := c.fetch(p, prev)
		c.mu.Lock()
		delete(c.inflight, p)
		c.mu.Unlock()
		cl.data, cl.err = data, err
		close(cl.done)
	})
	return cl
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

// fetch calls met.no for p if the rules allow it, and returns what waiting requests get.
func (c *Cache) fetch(p geo.Point, prev *entry) (Data, error) {
	ctx, cancel := context.WithTimeoutCause(c.ctx, upstreamTimeout, errUpstreamTimeout)
	defer cancel()
	now := time.Now()
	known := prev.hasData()
	if known && !prev.usable(now) && now.Before(prev.Expires) {
		// The data is past its 2-hour deadline, but the Terms of Service forbid calling met.no before Expires.
		if c.once(ctx, "expires:"+keyForecast(p), prev.Expires.Sub(now)) {
			c.log.Warn("location unavailable: met.no Expires is later than the 2-hour deadline",
				locationAttr(p), slog.Time("expires", prev.Expires))
		}
		return Data{}, unavailable(http.StatusServiceUnavailable, prev.Expires.Sub(now), "met.no's Expires has not passed")
	}

	pm, err := c.acquire(ctx, p)
	if errors.Is(err, errLeaseHeld) {
		if prev.usable(now) {
			return prev.data(now), nil
		}
		return c.awaitOther(ctx, p)
	}
	if err == nil {
		// Another call may have refreshed p since it was loaded.
		if cur, lerr := c.load(ctx, p); lerr == nil && cur.usable(now) && !now.Before(cur.ValidatedAt) &&
			(prev == nil || cur.ValidatedAt.After(prev.ValidatedAt)) {
			c.release(ctx, p)
			return cur.data(now), nil
		}
		err = c.spend(ctx, p, pm, known)
	}
	if err != nil {
		if prev.usable(now) {
			return prev.data(now), nil
		}
		if u, ok := errors.AsType[*UnavailableError](err); ok {
			return Data{}, u
		}
		return Data{}, c.storeUnavailable(err)
	}

	lastModified := ""
	if known {
		lastModified = prev.LastModified
	}
	f, err := c.fetcher.Get(ctx, p, lastModified)
	return c.complete(p, prev, pm, f, err)
}

// complete records the result of a met.no call.
func (c *Cache) complete(p geo.Point, prev *entry, pm permit, f metno.Forecast, err error) (Data, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), storeTimeout)
	defer cancel()
	now := time.Now()

	if err == nil {
		e := &entry{Steps: f.Steps, ValidatedAt: now, Expires: f.Expires, LastModified: f.LastModified}
		if f.NotModified {
			e.Steps = prev.Steps
		}
		refreshAfter := f.Expires
		if earliest := now.Add(minRefreshInterval); refreshAfter.Before(earliest) {
			refreshAfter = earliest
		}
		e.RefreshAfter = refreshAfter.Add(rand.N(refreshJitter))
		if serr := c.save(ctx, p, e); serr != nil {
			_ = c.storeUnavailable(serr) // the data itself is fine to serve
		}
		if _, serr := c.report(ctx, p, pm, nil, 0); serr != nil {
			_ = c.storeUnavailable(serr)
		}
		if f.Deprecated && c.once(ctx, "deprecated", deprecatedLogEach) {
			c.log.Warn("met.no says this product version is deprecated (203); migrate to a newer version")
		}
		return e.data(now), nil
	}

	maxBackoff := backoffMax
	if prev.usable(now) {
		maxBackoff = min(backoffMax, prev.deadline().Sub(now)/2)
	}
	retryAfter, serr := c.report(ctx, p, pm, err, maxBackoff)
	if serr != nil {
		_ = c.storeUnavailable(serr)
	}

	var fail *UnavailableError
	if _, ok := errors.AsType[*metno.ThrottledError](err); ok {
		c.log.Warn("met.no throttled us (429); pausing all calls", slog.Any("error", err))
		fail = unavailable(http.StatusServiceUnavailable, retryAfter, "met.no asked us to back off")
	} else if _, ok := errors.AsType[*metno.ForbiddenError](err); ok {
		c.log.Error("met.no refused our request (403); pausing all calls, a human must check this", slog.Any("error", err))
		fail = unavailable(http.StatusServiceUnavailable, retryAfter, "calls to met.no are paused")
	} else if _, ok := errors.AsType[*metno.ClientError](err); ok {
		c.log.Error("met.no rejected our request", locationAttr(p), slog.Any("error", err))
		fail = unavailable(http.StatusBadGateway, retryAfter, "met.no rejected the request")
	} else {
		c.log.Warn("met.no call failed", locationAttr(p), slog.Any("error", err))
		fail = unavailable(http.StatusBadGateway, retryAfter, "met.no failed")
	}

	// Data younger than 2 hours is still served.
	if prev.usable(now) {
		return prev.data(now), nil
	}
	return Data{}, fail
}

// awaitOther waits for another call, possibly on another instance, to fetch p.
func (c *Cache) awaitOther(ctx context.Context, p geo.Point) (Data, error) {
	ctx, cancel := context.WithTimeoutCause(ctx, waitTimeout, errWaitTimeout)
	defer cancel()
	for {
		select {
		case <-time.After(pollInterval):
		case <-ctx.Done():
			return Data{}, unavailable(http.StatusServiceUnavailable, time.Second, "a met.no call for this location is still in progress")
		}
		now := time.Now()
		e, err := c.load(ctx, p)
		if err != nil {
			return Data{}, c.storeUnavailable(err)
		}
		if e.usable(now) {
			return e.data(now), nil
		}
		heldUntil, err := c.getTime(ctx, keyLease(p))
		if err != nil {
			return Data{}, c.storeUnavailable(err)
		}
		if !now.Before(heldUntil) {
			// The other call ended without usable data, so it failed.
			b, _ := c.getBackoff(ctx, p)
			return Data{}, unavailable(http.StatusBadGateway, max(b.until.Sub(now), 0), "met.no failed")
		}
	}
}

func (c *Cache) load(ctx context.Context, p geo.Point) (*entry, error) {
	v, ok, err := c.store.get(ctx, keyForecast(p))
	if err != nil {
		return nil, err
	}
	if c.storeDown.CompareAndSwap(true, false) {
		c.log.Info("shared state is reachable again")
	}
	if !ok {
		return nil, nil
	}
	var e entry
	if err := json.Unmarshal([]byte(v), &e); err != nil {
		c.log.Warn("ignoring an unreadable cached forecast", locationAttr(p), slog.Any("error", err))
		return nil, nil
	}
	return &e, nil
}

func (c *Cache) save(ctx context.Context, p geo.Point, e *entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	// Keep the forecast as long as it may be served or decides when met.no may be called again.
	keep := e.deadline()
	for _, t := range []time.Time{e.Expires, e.RefreshAfter} {
		if t.After(keep) {
			keep = t
		}
	}
	return c.store.set(ctx, keyForecast(p), string(b), time.Until(keep)+time.Minute)
}

// storeUnavailable fails closed: without the shared state, no rule can be checked.
func (c *Cache) storeUnavailable(err error) error {
	if c.storeDown.CompareAndSwap(false, true) {
		c.log.Error("shared state is unreachable; failing closed", slog.Any("error", err))
	}
	return unavailable(http.StatusServiceUnavailable, storeRetryAfter, "shared state unavailable")
}

func (e *entry) hasData() bool { return e != nil && e.Steps != nil }

func (e *entry) deadline() time.Time { return e.ValidatedAt.Add(MaxAge) }

func (e *entry) usable(now time.Time) bool { return e.hasData() && now.Before(e.deadline()) }

func (e *entry) data(now time.Time) Data {
	freshFor := RefreshingFreshFor
	if now.Before(e.Expires) {
		freshFor = e.Expires.Sub(now)
	}
	return Data{Steps: e.Steps, ValidatedAt: e.ValidatedAt, Deadline: e.deadline(), FreshFor: freshFor}
}

func locationAttr(p geo.Point) slog.Attr {
	return slog.Group("location", slog.String("lat", p.Lat.String()), slog.String("lon", p.Lon.String()))
}
