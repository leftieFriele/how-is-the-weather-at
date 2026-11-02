package forecast_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

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

// Create it inside synctest.Test, so it runs on fake time.
type env struct {
	fake   *fakemetno.Server
	budget *forecast.Budget
	cache  *forecast.Cache
	cancel context.CancelFunc
}

func newEnv(t *testing.T) *env {
	t.Helper()
	fake := fakemetno.New()
	client, err := metno.NewClient(fake.Client(), metno.DefaultURL, "how-is-the-weather-at-api-test test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	budget := forecast.NewBudget(10)
	ctx, cancel := context.WithCancel(t.Context())
	cache, err := forecast.New(ctx, forecast.Config{
		Fetcher: client,
		Budget:  budget,
		Size:    1000,
		Logger:  slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		cache.Wait()
	})
	return &env{fake: fake, budget: budget, cache: cache, cancel: cancel}
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

func location(i int) geo.Point { return geo.Point{Lat: geo.Coord(599139 + i), Lon: 107522} }
