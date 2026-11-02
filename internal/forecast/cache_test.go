package forecast_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"how-is-the-weather-at-api/internal/forecast"
)

func TestForecastCache(t *testing.T) {
	oslo := location(0)

	behaviour(t, "serves cached data without calling met.no before Expires", func(t *testing.T, e *env) {
		// Given a location fetched now
		first := e.get(t, oslo)

		// When the event screen is opened again just before Expires
		time.Sleep(29 * time.Minute)
		second := e.get(t, oslo)

		// Then the cached data is served, and met.no was called once
		if !second.ValidatedAt.Equal(first.ValidatedAt) {
			t.Errorf("ValidatedAt = %s, want the cached %s", second.ValidatedAt, first.ValidatedAt)
		}
		if got := e.calls(); got != 1 {
			t.Errorf("met.no calls = %d, want 1", got)
		}
	})

	behaviour(t, "after Expires, serves cached data at once and refreshes it in the background with one met.no call", func(t *testing.T, e *env) {
		// Given a location fetched now
		first := e.get(t, oslo)

		// When Expires has passed and several requests come in
		time.Sleep(refreshDue)
		served := e.get(t, oslo)
		e.get(t, oslo)
		synctest.Wait()

		// Then the first response is the cached data, and one refresh was sent with If-Modified-Since
		if !served.ValidatedAt.Equal(first.ValidatedAt) {
			t.Errorf("ValidatedAt = %s, want the cached %s", served.ValidatedAt, first.ValidatedAt)
		}
		if served.FreshFor != forecast.RefreshingFreshFor {
			t.Errorf("FreshFor = %s, want %s while refreshing", served.FreshFor, forecast.RefreshingFreshFor)
		}
		reqs := e.fake.Requests()
		if len(reqs) != 2 {
			t.Fatalf("met.no calls = %d, want 2", len(reqs))
		}
		if reqs[1].IfModifiedSince == "" {
			t.Error("refresh sent no If-Modified-Since")
		}
		if after := e.get(t, oslo); !after.ValidatedAt.After(first.ValidatedAt) {
			t.Errorf("after the refresh ValidatedAt = %s, want later than %s", after.ValidatedAt, first.ValidatedAt)
		}
	})

	behaviour(t, "a 304 from met.no resets the 2-hour deadline", func(t *testing.T, e *env) {
		// Given a location fetched now, and refreshed after Expires with an unchanged forecast (304)
		first := e.get(t, oslo)
		time.Sleep(refreshDue)
		e.get(t, oslo)
		synctest.Wait()

		// When the first fetch's deadline has passed
		time.Sleep(time.Until(first.Deadline) + time.Second)
		d, err := e.cache.Get(t.Context(), oslo)

		// Then the data is still served, with a deadline 2 hours after the 304
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if d.Deadline.Before(first.Deadline.Add(refreshDue)) {
			t.Errorf("Deadline = %s, want at least %s", d.Deadline, first.Deadline.Add(refreshDue))
		}
		if !d.Deadline.Equal(d.ValidatedAt.Add(twoHours)) {
			t.Errorf("Deadline = %s, want ValidatedAt + 2h = %s", d.Deadline, d.ValidatedAt.Add(twoHours))
		}
	})

	behaviour(t, "never serves data after the 2-hour deadline, and fetches again instead", func(t *testing.T, e *env) {
		// Given a location fetched now, and no requests for 2 hours
		first := e.get(t, oslo)
		time.Sleep(twoHours + time.Second)

		// When the event screen is opened
		d := e.get(t, oslo)

		// Then met.no was called again before answering
		if !d.ValidatedAt.Equal(time.Now()) {
			t.Errorf("ValidatedAt = %s, want now (%s)", d.ValidatedAt, time.Now())
		}
		if !d.ValidatedAt.After(first.Deadline) || e.calls() != 2 {
			t.Errorf("met.no calls = %d, want 2", e.calls())
		}
	})

	behaviour(t, "never serves data after the 2-hour deadline, even when met.no fails", func(t *testing.T, e *env) {
		// Given a location fetched now, and met.no failing from then on
		e.get(t, oslo)
		e.failWith(http.StatusInternalServerError, nil)

		// When 2 hours pass and the event screen is opened
		time.Sleep(twoHours + time.Second)

		// Then the response is 502 and contains no weather data
		e.unavailable(t, oslo, http.StatusBadGateway)
	})

	behaviour(t, "never calls met.no before Expires, even after the deadline", func(t *testing.T, e *env) {
		// Given a location fetched now, with an Expires 3 hours ahead
		e.fake.SetExpiresIn(3 * time.Hour)
		e.get(t, oslo)

		// When the deadline has passed but Expires hasn't
		time.Sleep(twoHours + time.Second)
		u := e.unavailable(t, oslo, http.StatusServiceUnavailable)

		// Then the location is unavailable until Expires, and met.no was not called
		if want := time.Hour - time.Second; u.RetryAfter != want {
			t.Errorf("RetryAfter = %s, want %s", u.RetryAfter, want)
		}
		if got := e.calls(); got != 1 {
			t.Errorf("met.no calls = %d, want 1", got)
		}
	})

	behaviour(t, "many requests for the same location make one met.no call", func(t *testing.T, e *env) {
		// Given met.no takes a second to answer
		e.fake.SetDelay(time.Second)

		// When 50 users open the same new event at once
		var wg sync.WaitGroup
		var mu sync.Mutex
		var failed int
		for range 50 {
			wg.Go(func() {
				if _, err := e.cache.Get(t.Context(), oslo); err != nil {
					mu.Lock()
					failed++
					mu.Unlock()
				}
			})
		}
		wg.Wait()

		// Then all of them get data from one met.no call
		if failed != 0 {
			t.Errorf("%d requests failed, want 0", failed)
		}
		if got := e.calls(); got != 1 {
			t.Errorf("met.no calls = %d, want 1", got)
		}
	})

	behaviour(t, "a request that gives up after 3s doesn't cancel the met.no call, and the next request gets the result", func(t *testing.T, e *env) {
		// Given met.no takes 5 seconds to answer
		e.fake.SetDelay(5 * time.Second)

		// When the first request gives up after 3 seconds
		u := e.unavailable(t, oslo, http.StatusServiceUnavailable)
		if !strings.Contains(u.Reason, "gave up") {
			t.Errorf("Reason = %q, want it to say the request gave up", u.Reason)
		}
		time.Sleep(3 * time.Second)

		// Then the call finished anyway, and the next request is served from the cache
		e.get(t, oslo)
		if got := e.calls(); got != 1 {
			t.Errorf("met.no calls = %d, want 1", got)
		}
	})

	behaviour(t, "a failed background refresh keeps serving until the deadline, and retries before it", func(t *testing.T, e *env) {
		// Given a location fetched now, and met.no failing from then on
		first := e.get(t, oslo)
		e.failWith(http.StatusInternalServerError, nil)

		// When Expires passes and the background refresh fails
		time.Sleep(refreshDue)
		e.get(t, oslo)
		synctest.Wait()

		// Then the cached data is still served, without calling met.no during the backoff
		if d := e.get(t, oslo); !d.ValidatedAt.Equal(first.ValidatedAt) {
			t.Errorf("ValidatedAt = %s, want the cached %s", d.ValidatedAt, first.ValidatedAt)
		}
		if got := e.calls(); got != 2 {
			t.Errorf("met.no calls during backoff = %d, want 2", got)
		}

		// And met.no is tried again once the backoff (30s plus up to 20% jitter) is over
		time.Sleep(37 * time.Second)
		e.get(t, oslo)
		synctest.Wait()
		if got := e.calls(); got != 3 {
			t.Errorf("met.no calls after backoff = %d, want 3", got)
		}
	})

	behaviour(t, "shutdown cancels running met.no calls", func(t *testing.T, e *env) {
		// Given a met.no call that would take an hour
		e.fake.SetDelay(time.Hour)
		go func() { _, _ = e.cache.Get(t.Context(), oslo) }()
		synctest.Wait()

		// When the service shuts down
		start := time.Now()
		e.cancel()
		e.cache.Wait()

		// Then the call ends at once
		if waited := time.Since(start); waited > 0 {
			t.Errorf("shutdown waited %s, want no wait", waited)
		}
	})

	behaviour(t, "refreshes at most once a minute when met.no's Expires has already passed", func(t *testing.T, e *env) {
		// Given met.no sends an Expires that is already past, as with clock skew
		e.fake.SetExpiresIn(-time.Minute)
		e.get(t, oslo)

		// When the event screen is opened every second for a minute
		for range 59 {
			time.Sleep(time.Second)
			e.get(t, oslo)
			synctest.Wait()
		}

		// Then met.no is not called again
		if got := e.calls(); got != 1 {
			t.Fatalf("met.no calls within the first minute = %d, want 1", got)
		}

		// And once the minute and the jitter have passed, it is refreshed once
		time.Sleep(time.Minute + time.Second)
		e.get(t, oslo)
		synctest.Wait()
		if got := e.calls(); got != 2 {
			t.Errorf("met.no calls after the minute = %d, want 2", got)
		}
	})

	behaviour(t, "a 304 still serves the forecast when the cached copy disappeared during the call", func(t *testing.T, e *env) {
		// Given a refresh of Oslo that takes 2 seconds
		first := e.get(t, oslo)
		time.Sleep(refreshDue)
		e.fake.SetDelay(2 * time.Second)
		e.get(t, oslo)
		synctest.Wait()

		// When the cached copy disappears before met.no answers 304, as when Redis evicts it
		forecast.Forget(e.store, oslo)
		time.Sleep(2 * time.Second)
		synctest.Wait()

		// Then the forecast is served without another met.no call, confirmed by the 304
		d := e.get(t, oslo)
		if got := e.calls(); got != 2 {
			t.Errorf("met.no calls = %d, want 2", got)
		}
		if !d.ValidatedAt.After(first.ValidatedAt) || len(d.Steps) != len(first.Steps) {
			t.Errorf("ValidatedAt = %s with %d steps, want after %s with %d steps", d.ValidatedAt, len(d.Steps), first.ValidatedAt, len(first.Steps))
		}
	})

	behaviour(t, "warns once, not on every request, when Expires is later than the deadline", func(t *testing.T, e *env) {
		// Given a location whose deadline has passed before met.no's Expires
		e.fake.SetExpiresIn(3 * time.Hour)
		e.get(t, oslo)
		time.Sleep(twoHours + time.Second)

		// When the event screen is opened 10 times
		for range 10 {
			e.unavailable(t, oslo, http.StatusServiceUnavailable)
		}

		// Then the warning is logged once
		if got := e.logs.count("Expires is later than the 2-hour deadline"); got != 1 {
			t.Errorf("warnings logged = %d, want 1", got)
		}
	})
}
