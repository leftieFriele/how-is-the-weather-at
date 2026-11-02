package forecast_test

import (
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"how-is-the-weather-at-api/internal/forecast"
)

func TestUpstreamBudget(t *testing.T) {
	behaviour(t, "calls stay within the rate, and never more than 15 in any second", func(t *testing.T, e *env) {
		// Given a budget of 10 calls per second

		// When something tries to call met.no every millisecond for 10 seconds
		taken := hammer(e, 10*time.Second)

		// Then about 10 calls per second got through, plus the 5 tokens the bucket started with
		if n := len(taken); n < 100 || n > 105 {
			t.Errorf("calls = %d, want 100 to 105", n)
		}
		if n := busiestSecond(taken); n > 15 {
			t.Errorf("%d calls in one second, want at most 15", n)
		}
	})

	behaviourAtRate(t, "never lets more than 20 calls through in any second, even at the highest allowed rate", forecast.MaxRate, func(t *testing.T, e *env) {
		// Given a full budget at the highest rate the service accepts

		// When something tries to call met.no every millisecond for 10 seconds
		taken := hammer(e, 10*time.Second)

		// Then met.no's limit of 20 per second is never exceeded
		if n := busiestSecond(taken); n > 20 {
			t.Errorf("%d calls in one second, want at most 20", n)
		}
	})

	behaviour(t, "new locations cannot take the last 2 tokens, but known locations can", func(t *testing.T, e *env) {
		// Given a full budget of 5 tokens

		// When new locations take what they can, and then known locations
		var newTaken, knownTaken int
		for forecast.TakeToken(e.store, false) {
			newTaken++
		}
		for forecast.TakeToken(e.store, true) {
			knownTaken++
		}

		// Then new locations left 2 tokens, which known locations took
		if newTaken != 3 || knownTaken != 2 {
			t.Errorf("new took %d and known took %d, want 3 and 2", newTaken, knownTaken)
		}
	})
}

// hammer tries to take a token every millisecond for d, and returns when it got one.
func hammer(e *env, d time.Duration) []time.Time {
	var taken []time.Time
	for range int(d / time.Millisecond) {
		if forecast.TakeToken(e.store, true) {
			taken = append(taken, time.Now())
		}
		time.Sleep(time.Millisecond)
	}
	return taken
}

func TestRateLimiting(t *testing.T) {
	behaviour(t, "a request gets 503 with Retry-After at once when no token is free", func(t *testing.T, e *env) {
		// Given new locations have used all the tokens they may take
		for i := range 3 {
			e.get(t, location(i))
		}

		// When another new event is opened
		u := e.unavailable(t, location(3), http.StatusServiceUnavailable)

		// Then it gets Retry-After: 1, and met.no is not called for it
		if u.RetryAfter != time.Second {
			t.Errorf("RetryAfter = %s, want 1s", u.RetryAfter)
		}
		if got := e.calls(); got != 3 {
			t.Errorf("met.no calls = %d, want 3", got)
		}
	})

	behaviour(t, "a background refresh without a token is skipped, and the next request tries again", func(t *testing.T, e *env) {
		// Given a cached location whose refresh is due, and no tokens left
		e.get(t, location(0))
		time.Sleep(refreshDue)
		e.drainBudget()

		// When the event screen is opened
		e.get(t, location(0))
		synctest.Wait()

		// Then the cached data is served and no refresh is sent
		if got := e.calls(); got != 1 {
			t.Errorf("met.no calls = %d, want 1", got)
		}

		// And once a token is free again, the next request refreshes it
		time.Sleep(time.Second)
		e.get(t, location(0))
		synctest.Wait()
		if got := e.calls(); got != 2 {
			t.Errorf("met.no calls = %d, want 2", got)
		}
	})

	behaviour(t, "after a restart, new locations get nearly the whole budget", func(t *testing.T, e *env) {
		// Given an empty cache with a budget of 10 calls per second

		// When a new event is opened every 10 ms for 10 seconds
		served := 0
		for i := range 1000 {
			if _, err := e.cache.Get(t.Context(), location(i)); err == nil {
				served++
			}
			time.Sleep(10 * time.Millisecond)
		}

		// Then close to 10 per second got a forecast
		if served < 95 {
			t.Errorf("served %d new locations in 10s, want at least 95", served)
		}
	})

	behaviour(t, "a 429 pauses all calls for Retry-After", func(t *testing.T, e *env) {
		// Given met.no answers 429 with Retry-After: 120
		e.failWith(http.StatusTooManyRequests, http.Header{"Retry-After": {"120"}})
		e.unavailable(t, location(0), http.StatusServiceUnavailable)
		e.failWith(0, nil)

		// When another location is requested during the pause
		time.Sleep(time.Minute)
		u := e.unavailable(t, location(1), http.StatusServiceUnavailable)

		// Then it gets 503 with the remaining pause, without a met.no call
		if u.RetryAfter != time.Minute {
			t.Errorf("RetryAfter = %s, want 1m", u.RetryAfter)
		}
		if got := e.calls(); got != 1 {
			t.Errorf("met.no calls = %d, want 1", got)
		}

		// And after the pause, met.no is called again
		time.Sleep(time.Minute + time.Second)
		e.get(t, location(1))
	})

	behaviour(t, "a 403 pauses all calls, probes after 15 min, and doubles the pause", func(t *testing.T, e *env) {
		// Given met.no answers 403
		e.failWith(http.StatusForbidden, nil)
		e.unavailable(t, location(0), http.StatusServiceUnavailable)

		// When 15 minutes have passed, one probe is sent, and it gets 403 again
		time.Sleep(14 * time.Minute)
		e.unavailable(t, location(1), http.StatusServiceUnavailable)
		if got := e.calls(); got != 1 {
			t.Fatalf("met.no calls during the pause = %d, want 1", got)
		}
		time.Sleep(time.Minute + time.Second)
		e.unavailable(t, location(1), http.StatusServiceUnavailable)
		if got := e.calls(); got != 2 {
			t.Fatalf("met.no calls after the pause = %d, want 2 (the probe)", got)
		}

		// Then the next pause is 30 minutes
		time.Sleep(29 * time.Minute)
		e.unavailable(t, location(1), http.StatusServiceUnavailable)
		if got := e.calls(); got != 2 {
			t.Fatalf("met.no calls during the second pause = %d, want 2", got)
		}

		// And a successful probe ends the pause for everyone
		e.failWith(0, nil)
		time.Sleep(time.Minute + time.Second)
		e.get(t, location(1))
		e.get(t, location(2))
	})

	behaviour(t, "5 failures in a row pause all calls, and the first success ends the pause", func(t *testing.T, e *env) {
		// Given met.no fails for 5 different locations in a row
		e.failWith(http.StatusInternalServerError, nil)
		for i := range 5 {
			e.unavailable(t, location(i), http.StatusBadGateway)
			time.Sleep(time.Second)
		}

		// When another location is requested
		u := e.unavailable(t, location(5), http.StatusServiceUnavailable)

		// Then all calls are paused for 30s
		if got := e.calls(); got != 5 {
			t.Errorf("met.no calls = %d, want 5", got)
		}
		if u.RetryAfter <= 0 || u.RetryAfter > 30*time.Second {
			t.Errorf("RetryAfter = %s, want up to 30s", u.RetryAfter)
		}

		// And once met.no works again after the pause, calls go through as normal
		e.failWith(0, nil)
		time.Sleep(30 * time.Second)
		e.get(t, location(5))
		time.Sleep(time.Second)
		e.get(t, location(6))
	})

	behaviour(t, "a location in backoff gets 502 without a met.no call", func(t *testing.T, e *env) {
		// Given a fetch for a new location failed
		e.failWith(http.StatusInternalServerError, nil)
		e.unavailable(t, location(0), http.StatusBadGateway)
		e.failWith(0, nil)

		// When it is requested again straight away
		u := e.unavailable(t, location(0), http.StatusBadGateway)

		// Then it gets 502 with Retry-After, and met.no is not called
		if u.RetryAfter < 30*time.Second {
			t.Errorf("RetryAfter = %s, want at least 30s", u.RetryAfter)
		}
		if got := e.calls(); got != 1 {
			t.Errorf("met.no calls = %d, want 1", got)
		}

		// And after the backoff, it is fetched again
		time.Sleep(u.RetryAfter)
		e.get(t, location(0))
	})

	behaviour(t, "a probe is still sent after a 403 pause that ends while no token is free", func(t *testing.T, e *env) {
		// Given met.no answered 403, and the pause has just ended
		e.failWith(http.StatusForbidden, nil)
		e.unavailable(t, location(0), http.StatusServiceUnavailable)
		e.failWith(0, nil)
		time.Sleep(15*time.Minute + time.Second)

		// When the first request after the pause finds no token free
		e.drainBudget()
		e.unavailable(t, location(1), http.StatusServiceUnavailable)

		// Then the probe goes out once a token is free, and calls work again
		time.Sleep(time.Second)
		e.get(t, location(1))
		e.get(t, location(2))
	})
}
