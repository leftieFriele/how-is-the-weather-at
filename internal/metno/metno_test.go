package metno_test

import (
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"how-is-the-weather-at-api/internal/fakemetno"
	"how-is-the-weather-at-api/internal/geo"
	"how-is-the-weather-at-api/internal/metno"
)

const userAgent = "how-is-the-weather-at-api-test test@example.com"

var oslo = geo.Point{Lat: 599139, Lon: 107522}

func newClient(t *testing.T, h http.Handler) *metno.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := metno.NewClient(srv.Client(), srv.URL+"/weatherapi/locationforecast/2.0/compact", userAgent)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMetnoClient(t *testing.T) {
	t.Run("identifies itself, asks for gzip and sends coordinates with at most 4 decimals", func(t *testing.T) {
		// Given
		fake := fakemetno.New()
		c := newClient(t, fake)

		// When
		f, err := c.Get(t.Context(), oslo, "")

		// Then
		if err != nil {
			t.Fatal(err)
		}
		req := fake.Requests()[0]
		if req.UserAgent != userAgent {
			t.Errorf("User-Agent = %q, want %q", req.UserAgent, userAgent)
		}
		if !strings.Contains(req.AcceptEncoding, "gzip") {
			t.Errorf("Accept-Encoding = %q, want gzip", req.AcceptEncoding)
		}
		if req.Lat != "59.9139" || req.Lon != "10.7522" {
			t.Errorf("coordinates = %s,%s, want 59.9139,10.7522", req.Lat, req.Lon)
		}
		if len(f.Steps) == 0 || f.LastModified == "" || f.Expires.IsZero() {
			t.Errorf("forecast = %d steps, Last-Modified %q, Expires %s; want all set", len(f.Steps), f.LastModified, f.Expires)
		}
	})

	t.Run("sends If-Modified-Since with the previous Last-Modified and reports an unchanged forecast", func(t *testing.T) {
		// Given
		fake := fakemetno.New()
		c := newClient(t, fake)
		first, err := c.Get(t.Context(), oslo, "")
		if err != nil {
			t.Fatal(err)
		}

		// When
		second, err := c.Get(t.Context(), oslo, first.LastModified)

		// Then
		if err != nil {
			t.Fatal(err)
		}
		if got := fake.Requests()[1].IfModifiedSince; got != first.LastModified {
			t.Errorf("If-Modified-Since = %q, want %q", got, first.LastModified)
		}
		if !second.NotModified || second.Expires.IsZero() {
			t.Errorf("NotModified = %v, Expires = %s; want true and set", second.NotModified, second.Expires)
		}
	})

	t.Run("turns each met.no status into the right result", func(t *testing.T) {
		tests := []struct {
			name   string
			status int
			header http.Header
			then   func(t *testing.T, f metno.Forecast, err error)
		}{
			{"203 is data marked as deprecated", http.StatusNonAuthoritativeInfo, nil, func(t *testing.T, f metno.Forecast, err error) {
				if err != nil || !f.Deprecated || len(f.Steps) == 0 {
					t.Errorf("Deprecated = %v, %d steps, err = %v; want deprecated data", f.Deprecated, len(f.Steps), err)
				}
			}},
			{"429 is throttled with Retry-After", http.StatusTooManyRequests, http.Header{"Retry-After": {"120"}}, func(t *testing.T, _ metno.Forecast, err error) {
				if e, ok := errors.AsType[*metno.ThrottledError](err); !ok || e.RetryAfter != 2*time.Minute {
					t.Errorf("err = %v, want ThrottledError with 2m", err)
				}
			}},
			{"429 without Retry-After waits 60s", http.StatusTooManyRequests, nil, func(t *testing.T, _ metno.Forecast, err error) {
				if e, ok := errors.AsType[*metno.ThrottledError](err); !ok || e.RetryAfter != time.Minute {
					t.Errorf("err = %v, want ThrottledError with 1m", err)
				}
			}},
			{"403 is forbidden", http.StatusForbidden, nil, func(t *testing.T, _ metno.Forecast, err error) {
				if _, ok := errors.AsType[*metno.ForbiddenError](err); !ok {
					t.Errorf("err = %v, want ForbiddenError", err)
				}
			}},
			{"404 is a client error", http.StatusNotFound, nil, func(t *testing.T, _ metno.Forecast, err error) {
				if e, ok := errors.AsType[*metno.ClientError](err); !ok || e.Status != http.StatusNotFound {
					t.Errorf("err = %v, want ClientError 404", err)
				}
			}},
			{"500 is an upstream error", http.StatusInternalServerError, nil, func(t *testing.T, _ metno.Forecast, err error) {
				if e, ok := errors.AsType[*metno.UpstreamError](err); !ok || e.Status != http.StatusInternalServerError {
					t.Errorf("err = %v, want UpstreamError 500", err)
				}
			}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				// Given
				c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					maps.Copy(w.Header(), tt.header)
					w.WriteHeader(tt.status)
					if tt.status == http.StatusNonAuthoritativeInfo {
						_, _ = w.Write([]byte(`{"properties":{"timeseries":[{"time":"2026-11-02T09:00:00Z","data":{"instant":{"details":{"air_temperature":4.2,"wind_speed":3.1}}}}]}}`))
					}
				}))

				// When
				f, err := c.Get(t.Context(), oslo, "")

				// Then
				tt.then(t, f, err)
			})
		}
	})

	t.Run("reports a network error as an upstream error", func(t *testing.T) {
		// Given a server that is gone
		srv := httptest.NewServer(http.NotFoundHandler())
		srv.Close()
		c, _ := metno.NewClient(srv.Client(), srv.URL, userAgent)

		// When
		_, err := c.Get(t.Context(), oslo, "")

		// Then
		if e, ok := errors.AsType[*metno.UpstreamError](err); !ok || e.Status != 0 {
			t.Errorf("err = %v, want UpstreamError without status", err)
		}
	})
}
