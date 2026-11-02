package forecast_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"how-is-the-weather-at-api/internal/fakemetno"
	"how-is-the-weather-at-api/internal/forecast"
	"how-is-the-weather-at-api/internal/geo"
	"how-is-the-weather-at-api/internal/metno"
)

// twoHours is the freshness limit from the requirements. Tests use it rather than forecast.MaxAge, so changing the
// constant can't make them pass by accident.
const twoHours = 2 * time.Hour

// refreshDue is long enough after a fetch for met.no's Expires (30 min) plus the refresh jitter (up to 60s) to pass.
const refreshDue = fakemetno.DefaultExpiresIn + time.Minute + time.Second

const defaultRate = 10

// behaviour runs fn against both stores, on fake time: once in memory, and once against Redis (miniredis).
func behaviour(t *testing.T, name string, fn func(t *testing.T, e *env)) {
	t.Helper()
	behaviourAtRate(t, name, defaultRate, fn)
}

func behaviourAtRate(t *testing.T, name string, rate float64, fn func(t *testing.T, e *env)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Run("in memory", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store, err := forecast.NewMemoryStore(rate, 1000)
				if err != nil {
					t.Fatal(err)
				}
				fn(t, newEnv(t, store))
			})
		})
		t.Run("redis", func(t *testing.T) {
			// Started outside the synctest bubble, so its goroutines don't hold up fake time.
			mr := miniredis.RunT(t)
			synctest.Test(t, func(t *testing.T) {
				fn(t, newEnv(t, redisStore(t, mr, rate)))
			})
		})
	})
}

func redisStore(t *testing.T, mr *miniredis.Miniredis, rate float64) forecast.Store {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return forecast.NewRedisStore(rdb, rate)
}

type env struct {
	fake   *fakemetno.Server
	store  forecast.Store
	cache  *forecast.Cache
	cancel context.CancelFunc
	logs   *logBuffer
}

// Create it inside synctest.Test, so it runs on fake time.
func newEnv(t *testing.T, store forecast.Store) *env {
	t.Helper()
	fake := fakemetno.New()
	cache, cancel, logs := newCache(t, fake, store)
	return &env{fake: fake, store: store, cache: cache, cancel: cancel, logs: logs}
}

func newCache(t *testing.T, fake *fakemetno.Server, store forecast.Store) (*forecast.Cache, context.CancelFunc, *logBuffer) {
	t.Helper()
	client, err := metno.NewClient(fake.Client(), metno.DefaultURL, "how-is-the-weather-at-api-test test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	ctx, cancel := context.WithCancel(t.Context())
	cache := forecast.New(ctx, forecast.Config{
		Fetcher: client,
		Store:   store,
		Logger:  slog.New(slog.NewTextHandler(logs, nil)),
	})
	t.Cleanup(func() {
		cancel()
		cache.Wait()
	})
	return cache, cancel, logs
}

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) count(msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.buf.String(), msg)
}

func (e *env) get(t *testing.T, p geo.Point) forecast.Data {
	t.Helper()
	d, err := e.cache.Get(t.Context(), p)
	if err != nil {
		t.Fatalf("Get(%v): %v", p, err)
	}
	return d
}

func (e *env) unavailable(t *testing.T, p geo.Point, status int) forecast.UnavailableError {
	t.Helper()
	d, err := e.cache.Get(t.Context(), p)
	u, ok := errors.AsType[*forecast.UnavailableError](err)
	if !ok {
		t.Fatalf("Get(%v) = %d steps, %v; want unavailable (%d)", p, len(d.Steps), err, status)
	}
	if u.Status != status {
		t.Fatalf("Get(%v) status = %d (%s), want %d", p, u.Status, u.Reason, status)
	}
	return *u
}

func (e *env) calls() int { return len(e.fake.Requests()) }

func (e *env) failWith(status int, header http.Header) { e.fake.SetStatus(status, header) }

func (e *env) drainBudget() {
	for forecast.TakeToken(e.store, true) {
	}
}

func location(i int) geo.Point { return geo.Point{Lat: geo.Coord(599139 + i), Lon: 107522} }

// busiestSecond returns the most calls within any one second.
func busiestSecond(times []time.Time) int {
	most := 0
	for i, start := range times {
		n := 0
		for _, at := range times[i:] {
			if at.Sub(start) < time.Second {
				n++
			}
		}
		most = max(most, n)
	}
	return most
}
