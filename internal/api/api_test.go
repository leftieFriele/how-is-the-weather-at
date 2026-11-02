package api_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"testing/synctest"
	"time"

	"how-is-the-weather-at-api/internal/api"
	"how-is-the-weather-at-api/internal/forecast"
	"how-is-the-weather-at-api/internal/geo"
	"how-is-the-weather-at-api/internal/metno"
)

type stubForecasts struct {
	data     forecast.Data
	err      error
	calls    int
	notReady error
}

func (s *stubForecasts) Get(context.Context, geo.Point) (forecast.Data, error) {
	s.calls++
	return s.data, s.err
}

func (s *stubForecasts) Ready(context.Context) error { return s.notReady }

// Like met.no: hourly for 60 hours, then 6-hourly up to 9 days.
func steps() []metno.Step {
	start := time.Now().UTC().Truncate(time.Hour)
	var out []metno.Step
	for t := start; t.Before(start.Add(9 * 24 * time.Hour)); {
		out = append(out, metno.Step{Time: t, AirTemperature: 4.2, WindSpeed: 3.1})
		if t.Sub(start) < 59*time.Hour {
			t = t.Add(time.Hour)
		} else {
			t = t.Truncate(6 * time.Hour).Add(6 * time.Hour)
		}
	}
	return out
}

// Data met.no confirmed age ago, with Expires freshFor from now.
func fetched(age, freshFor time.Duration) forecast.Data {
	validated := time.Now().Add(-age)
	return forecast.Data{Steps: steps(), ValidatedAt: validated, Deadline: validated.Add(2 * time.Hour), FreshFor: freshFor}
}

type response struct {
	Phase    string `json:"phase"`
	Location struct {
		Lat float64 `json:"lat"`
		Lon float64 `json:"lon"`
	} `json:"location"`
	Current *struct {
		Time time.Time `json:"time"`
	} `json:"current"`
	Forecast []struct {
		Time time.Time `json:"time"`
	} `json:"forecast"`
	AvailableFrom *time.Time `json:"available_from"`
	FetchedAt     *time.Time `json:"fetched_at"`
	Error         string     `json:"error"`
}

func open(t *testing.T, f api.Forecaster, lat string, start, end time.Time) (*httptest.ResponseRecorder, response) {
	t.Helper()
	q := url.Values{
		"lat":   {lat},
		"lon":   {"10.7522"},
		"start": {start.Format(time.RFC3339)},
		"end":   {end.Format(time.RFC3339)},
	}
	rec := httptest.NewRecorder()
	api.New(f, slog.New(slog.DiscardHandler)).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/events/abc123/weather?"+q.Encode(), nil))
	var body response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON %q: %v", rec.Body.String(), err)
	}
	return rec, body
}

func at(d time.Duration) time.Time { return time.Now().Add(d) }

func TestResponseHeaders(t *testing.T) {
	t.Run("never lets a copy live past the 2-hour deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// Given data confirmed 1h50m ago, so 10 minutes are left before its deadline
			stub := &stubForecasts{data: fetched(110*time.Minute, 20*time.Minute)}

			// When an event in two days is opened
			rec, _ := open(t, stub, "59.9139", at(48*time.Hour), at(50*time.Hour))

			// Then max-age plus stale-while-revalidate ends at the deadline
			if got, want := rec.Header().Get("Cache-Control"), "public, max-age=600, stale-while-revalidate=0"; got != want {
				t.Errorf("Cache-Control = %q, want %q", got, want)
			}
		})
	})

	t.Run("lets fresh data be cached until met.no's Expires", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// Given data confirmed now, with Expires in 20 minutes
			stub := &stubForecasts{data: fetched(0, 20*time.Minute)}

			// When an event in two days is opened
			rec, _ := open(t, stub, "59.9139", at(48*time.Hour), at(50*time.Hour))

			// Then
			if got, want := rec.Header().Get("Cache-Control"), "public, max-age=1200, stale-while-revalidate=300"; got != want {
				t.Errorf("Cache-Control = %q, want %q", got, want)
			}
			if got := rec.Header().Get("Link"); got != `<https://creativecommons.org/licenses/by/4.0/>; rel="license"` {
				t.Errorf("Link = %q, want the CC BY 4.0 license", got)
			}
		})
	})

	t.Run("expires a copy when the event goes live", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// Given fresh data
			stub := &stubForecasts{data: fetched(0, 20*time.Minute)}

			// When an event starting in 5 minutes is opened
			rec, _ := open(t, stub, "59.9139", at(5*time.Minute), at(2*time.Hour))

			// Then
			if got, want := rec.Header().Get("Cache-Control"), "public, max-age=300, stale-while-revalidate=300"; got != want {
				t.Errorf("Cache-Control = %q, want %q", got, want)
			}
		})
	})

	t.Run("lets an ended event be cached for a day, without looking up the weather", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// Given
			stub := &stubForecasts{}

			// When an event that ended yesterday is opened
			rec, body := open(t, stub, "63.430512", at(-26*time.Hour), at(-24*time.Hour))

			// Then
			if got, want := rec.Header().Get("Cache-Control"), "public, max-age=86400, stale-while-revalidate=0"; got != want {
				t.Errorf("Cache-Control = %q, want %q", got, want)
			}
			if body.Phase != "ended" || body.Current != nil || body.Forecast == nil || len(body.Forecast) != 0 || body.FetchedAt != nil {
				t.Errorf("body = %+v, want ended with current null and forecast []", body)
			}
			if body.Location.Lat != 63.4305 {
				t.Errorf("location lat = %v, want the truncated 63.4305", body.Location.Lat)
			}
			if stub.calls != 0 {
				t.Errorf("weather looked up %d times, want 0", stub.calls)
			}
		})
	})

	t.Run("lets an event more than 7 days away be cached until its forecast is available", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// Given
			stub := &stubForecasts{}

			// When an event in 10 days is opened
			rec, body := open(t, stub, "59.9139", at(10*24*time.Hour), at(10*24*time.Hour+2*time.Hour))

			// Then
			if got, want := rec.Header().Get("Cache-Control"), "public, max-age=259200, stale-while-revalidate=0"; got != want {
				t.Errorf("Cache-Control = %q, want %q", got, want)
			}
			if body.Phase != "forecast_pending" || body.AvailableFrom == nil || !body.AvailableFrom.Equal(at(3*24*time.Hour)) {
				t.Errorf("body = %+v, want forecast_pending available in 3 days", body)
			}
			if stub.calls != 0 {
				t.Errorf("weather looked up %d times, want 0", stub.calls)
			}
		})
	})

	t.Run("sends no-store, Retry-After and a JSON error when the weather is unavailable", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// Given
			stub := &stubForecasts{err: &forecast.UnavailableError{Status: http.StatusServiceUnavailable, RetryAfter: 1500 * time.Millisecond, Reason: "no upstream budget left"}}

			// When
			rec, body := open(t, stub, "59.9139", at(48*time.Hour), at(50*time.Hour))

			// Then
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Retry-After") != "2" {
				t.Errorf("status %d, Cache-Control %q, Retry-After %q; want 503, no-store, 2",
					rec.Code, rec.Header().Get("Cache-Control"), rec.Header().Get("Retry-After"))
			}
			if body.Error == "" {
				t.Error("error message is empty")
			}
		})
	})
}

func TestWeatherRequest(t *testing.T) {
	t.Run("rejects invalid input with 400 and names the parameter", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// When
			rec, body := open(t, &stubForecasts{}, "5.99e1", at(48*time.Hour), at(50*time.Hour))

			// Then
			if rec.Code != http.StatusBadRequest || body.Error != "lat: must be a plain decimal number" {
				t.Errorf("status %d, error %q; want 400 and the lat message", rec.Code, body.Error)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	})

	t.Run("picks the forecast steps around the event", func(t *testing.T) {
		day := 24 * time.Hour
		tests := []struct {
			name       string
			start, end time.Duration // from now (midnight UTC in the test)
			then       []time.Duration
		}{
			{"hourly data, 18:00 to 20:00", 18 * time.Hour, 20 * time.Hour, []time.Duration{18 * time.Hour, 19 * time.Hour, 20 * time.Hour}},
			{"hourly data, between two steps", 18*time.Hour + 15*time.Minute, 18*time.Hour + 45*time.Minute, []time.Duration{18 * time.Hour, 19 * time.Hour}},
			{"6-hourly data, 19:00 to 21:00", 4*day + 19*time.Hour, 4*day + 21*time.Hour, []time.Duration{4*day + 18*time.Hour, 5 * day}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					// Given
					stub := &stubForecasts{data: fetched(0, 20*time.Minute)}

					// When
					_, body := open(t, stub, "59.9139", at(tt.start), at(tt.end))

					// Then
					var got []time.Duration
					for _, f := range body.Forecast {
						got = append(got, time.Until(f.Time))
					}
					if len(got) != len(tt.then) {
						t.Fatalf("steps at %v, want %v", got, tt.then)
					}
					for i := range got {
						if got[i] != tt.then[i] {
							t.Fatalf("steps at %v, want %v", got, tt.then)
						}
					}
				})
			})
		}
	})

	t.Run("gives a live event the weather now and the steps until it ends", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// Given half an hour has passed since a cached fetch at midnight
			stub := &stubForecasts{data: fetched(0, 20*time.Minute)}
			time.Sleep(30 * time.Minute)

			// When an event that started an hour ago and ends in 90 minutes is opened
			_, body := open(t, stub, "59.9139", at(-time.Hour), at(90*time.Minute))

			// Then current is the step at midnight, and the forecast runs from midnight to 02:00
			midnight := time.Now().Truncate(time.Hour)
			if body.Phase != "live" || body.Current == nil || !body.Current.Time.Equal(midnight) {
				t.Errorf("phase %s, current %+v; want live with current at %s", body.Phase, body.Current, midnight)
			}
			if n := len(body.Forecast); n != 3 || !body.Forecast[0].Time.Equal(midnight) || !body.Forecast[n-1].Time.Equal(midnight.Add(2*time.Hour)) {
				t.Errorf("forecast = %+v, want 00:00, 01:00 and 02:00", body.Forecast)
			}
		})
	})

	t.Run("answers other methods with 405", func(t *testing.T) {
		// When
		rec := httptest.NewRecorder()
		api.New(&stubForecasts{}, slog.New(slog.DiscardHandler)).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/events/abc123/weather", nil))

		// Then
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", rec.Code)
		}
	})
}

func TestHealthChecks(t *testing.T) {
	check := func(t *testing.T, s *api.Server, path string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		return rec.Code
	}

	t.Run("is ready while the shared state can be reached", func(t *testing.T) {
		// Given
		s := api.New(&stubForecasts{}, slog.New(slog.DiscardHandler))

		// Then
		if got := check(t, s, "/readyz"); got != http.StatusOK {
			t.Errorf("/readyz = %d, want 200", got)
		}
	})

	t.Run("is not ready when the shared state can't be reached, but stays alive", func(t *testing.T) {
		// Given
		s := api.New(&stubForecasts{notReady: errors.New("connection refused")}, slog.New(slog.DiscardHandler))

		// Then
		if got := check(t, s, "/readyz"); got != http.StatusServiceUnavailable {
			t.Errorf("/readyz = %d, want 503", got)
		}
		if got := check(t, s, "/healthz"); got != http.StatusOK {
			t.Errorf("/healthz = %d, want 200", got)
		}
	})

	t.Run("is not ready once shutdown has started", func(t *testing.T) {
		// Given
		s := api.New(&stubForecasts{}, slog.New(slog.DiscardHandler))

		// When
		s.ShuttingDown()

		// Then
		if got := check(t, s, "/readyz"); got != http.StatusServiceUnavailable {
			t.Errorf("/readyz = %d, want 503", got)
		}
	})
}
