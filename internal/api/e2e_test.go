package api_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"

	"how-is-the-weather-at-api/internal/api"
	"how-is-the-weather-at-api/internal/fakemetno"
	"how-is-the-weather-at-api/internal/forecast"
	"how-is-the-weather-at-api/internal/metno"
)

// Real HTTP on both sides, unlike the in-memory tests.
func startService(t *testing.T) *httptest.Server {
	t.Helper()
	fake := httptest.NewServer(fakemetno.New())
	t.Cleanup(fake.Close)
	client, err := metno.NewClient(fake.Client(), fake.URL+"/weatherapi/locationforecast/2.0/compact", "how-is-the-weather-at-api-test test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cache, err := forecast.New(ctx, forecast.Config{
		Fetcher: client,
		Budget:  forecast.NewBudget(10),
		Size:    100,
		Logger:  slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(cache, slog.New(slog.DiscardHandler)))
	t.Cleanup(func() {
		srv.Close()
		cancel()
		cache.Wait()
	})
	return srv
}

// Matches requests on any host, not only the spec's localhost:8080.
func loadSpec(t *testing.T) routers.Router {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile("../../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(t.Context()); err != nil {
		t.Fatalf("openapi.yaml is invalid: %v", err)
	}
	doc.Servers = nil
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func TestEndToEnd(t *testing.T) {
	srv := startService(t)
	spec := loadSpec(t)
	now := time.Now().UTC()

	tests := []struct {
		name       string
		lat        string
		start, end time.Time
		then       int
	}{
		{"an upcoming event gets a forecast", "59.9139", now.Add(48 * time.Hour), now.Add(50 * time.Hour), http.StatusOK},
		{"a live event gets a forecast", "59.91", now.Add(-time.Hour), now.Add(time.Hour), http.StatusOK},
		{"an event more than 7 days away gets no forecast yet", "60.3913", now.Add(10 * 24 * time.Hour), now.Add(10*24*time.Hour + 2*time.Hour), http.StatusOK},
		{"an ended event gets no forecast", "63.430512", now.Add(-26 * time.Hour), now.Add(-24 * time.Hour), http.StatusOK},
		{"invalid input gets 400", "5.99e1", now.Add(48 * time.Hour), now.Add(50 * time.Hour), http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name+", matching openapi.yaml", func(t *testing.T) {
			// Given
			q := url.Values{
				"lat":   {tt.lat},
				"lon":   {"10.7522"},
				"start": {tt.start.Format(time.RFC3339)},
				"end":   {tt.end.Format(time.RFC3339)},
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/events/abc123/weather?"+q.Encode(), nil)
			if err != nil {
				t.Fatal(err)
			}

			// When
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}

			// Then
			if resp.StatusCode != tt.then {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tt.then, body)
			}
			route, params, err := spec.FindRoute(req)
			if err != nil {
				t.Fatal(err)
			}
			err = openapi3filter.ValidateResponse(t.Context(), &openapi3filter.ResponseValidationInput{
				RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
				Status:                 resp.StatusCode,
				Header:                 resp.Header,
				Body:                   io.NopCloser(bytes.NewReader(body)),
				Options:                &openapi3filter.Options{IncludeResponseStatus: true},
			})
			if err != nil {
				t.Errorf("response doesn't match openapi.yaml: %v\n%s", err, body)
			}
		})
	}
}
