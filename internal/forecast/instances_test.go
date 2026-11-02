package forecast_test

import (
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alicebob/miniredis/v2"

	"how-is-the-weather-at-api/internal/fakemetno"
	"how-is-the-weather-at-api/internal/forecast"
)

// Several instances sharing one Redis must behave like one service towards met.no.
func TestInstances(t *testing.T) {
	instances := func(t *testing.T, mr *miniredis.Miniredis, fake *fakemetno.Server, rate float64) []*forecast.Cache {
		t.Helper()
		var caches []*forecast.Cache
		for range 3 {
			cache, _, _ := newCache(t, fake, redisStore(t, mr, rate))
			caches = append(caches, cache)
		}
		return caches
	}

	t.Run("together never call met.no more than 20 times in any second", func(t *testing.T) {
		mr := miniredis.RunT(t)
		synctest.Test(t, func(t *testing.T) {
			// Given three instances at the highest rate the service accepts
			fake := fakemetno.New()
			caches := instances(t, mr, fake, forecast.MaxRate)

			// When each instance gets a new event every 10 ms for 10 seconds
			var wg sync.WaitGroup
			for i, cache := range caches {
				wg.Go(func() {
					for j := range 1000 {
						_, _ = cache.Get(t.Context(), location(i*1000+j))
						time.Sleep(10 * time.Millisecond)
					}
				})
			}
			wg.Wait()

			// Then met.no's limit holds for all of them together, and the budget was used
			var times []time.Time
			for _, r := range fake.Requests() {
				times = append(times, r.Time)
			}
			if n := busiestSecond(times); n > 20 {
				t.Errorf("%d met.no calls in one second, want at most 20", n)
			}
			if len(times) < 100 {
				t.Errorf("met.no calls = %d, want the budget to be used", len(times))
			}
		})
	})

	t.Run("together make one met.no call per location", func(t *testing.T) {
		mr := miniredis.RunT(t)
		synctest.Test(t, func(t *testing.T) {
			// Given three instances, and met.no taking a second to answer
			fake := fakemetno.New()
			fake.SetDelay(time.Second)
			caches := instances(t, mr, fake, defaultRate)

			// When 10 users per instance open the same new event at once
			var wg sync.WaitGroup
			var mu sync.Mutex
			failed := 0
			for _, cache := range caches {
				for range 10 {
					wg.Go(func() {
						if _, err := cache.Get(t.Context(), location(0)); err != nil {
							mu.Lock()
							failed++
							mu.Unlock()
						}
					})
				}
			}
			wg.Wait()

			// Then one instance called met.no, and the others picked up its result
			if got := len(fake.Requests()); got != 1 {
				t.Errorf("met.no calls = %d, want 1", got)
			}
			if failed != 0 {
				t.Errorf("%d requests failed, want 0", failed)
			}
		})
	})

	t.Run("together send one probe after a 403 pause", func(t *testing.T) {
		mr := miniredis.RunT(t)
		synctest.Test(t, func(t *testing.T) {
			// Given met.no answered 403 to one instance, and the 15 minute pause has passed
			fake := fakemetno.New()
			fake.SetStatus(http.StatusForbidden, nil)
			caches := instances(t, mr, fake, defaultRate)
			_, _ = caches[0].Get(t.Context(), location(0))
			time.Sleep(15*time.Minute + time.Second)

			// When all instances get new events at once
			var wg sync.WaitGroup
			for i, cache := range caches {
				for j := range 3 {
					wg.Go(func() { _, _ = cache.Get(t.Context(), location(1+i*3+j)) })
				}
			}
			wg.Wait()

			// Then only one of them called met.no, as the probe
			if got := len(fake.Requests()); got != 2 {
				t.Errorf("met.no calls = %d, want 2: the first 403 and one probe", got)
			}
		})
	})
}
