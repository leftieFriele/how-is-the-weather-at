package event_test

import (
	"maps"
	"net/url"
	"testing"
	"time"

	"how-is-the-weather-at-api/internal/event"
)

func query(start, end string) url.Values {
	return url.Values{"lat": {"59.9139"}, "lon": {"10.7522"}, "start": {start}, "end": {end}}
}

func TestEventRequest(t *testing.T) {
	t.Run("rejects invalid input and names the parameter", func(t *testing.T) {
		valid := query("2026-11-05T18:00:00+01:00", "2026-11-05T23:00:00+01:00")
		tests := []struct {
			name  string
			id    string
			given func(url.Values)
			then  string
		}{
			{"missing parameter", "abc", func(q url.Values) { q.Del("lat") }, "lat: is required"},
			{"duplicated parameter", "abc", func(q url.Values) { q.Add("lon", "10") }, "lon: must be given once"},
			{"exponent", "abc", func(q url.Values) { q.Set("lat", "5.99e1") }, "lat: must be a plain decimal number"},
			{"time without offset", "abc", func(q url.Values) { q.Set("start", "2026-11-05T18:00:00") }, "start: must be an RFC 3339 time with offset"},
			{"offset sent as a space", "abc", func(q url.Values) { q.Set("start", "2026-11-05T18:00:00 01:00") }, "start: must be an RFC 3339 time with offset"},
			{"end before start", "abc", func(q url.Values) { q.Set("end", "2026-11-05T17:00:00+01:00") }, "end: must be after start"},
			{"event longer than 14 days", "abc", func(q url.Values) { q.Set("end", "2026-11-25T18:00:00+01:00") }, "end: must be at most 14 days after start"},
			{"invalid event id", "abc/def", func(url.Values) {}, "eventId: must be 1 to 64 letters, digits, '-' or '_'"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				// Given
				q := maps.Clone(valid)
				tt.given(q)

				// When
				_, err := event.Parse(tt.id, q)

				// Then
				if err == nil || err.Error() != tt.then {
					t.Errorf("error = %v, want %q", err, tt.then)
				}
			})
		}
	})

	t.Run("decides the phase from start, end and the current time", func(t *testing.T) {
		// Given an event on 10 November, 18:00 to 20:00 UTC
		r, err := event.Parse("abc", query("2026-11-10T18:00:00Z", "2026-11-10T20:00:00Z"))
		if err != nil {
			t.Fatal(err)
		}
		tests := []struct {
			now  string
			then event.Phase
		}{
			{"2026-11-03T17:59:59Z", event.PhaseForecastPending},
			{"2026-11-03T18:00:00Z", event.PhaseUpcoming},
			{"2026-11-10T18:00:00Z", event.PhaseLive},
			{"2026-11-10T20:00:00Z", event.PhaseEnded},
		}
		for _, tt := range tests {
			now, _ := time.Parse(time.RFC3339, tt.now)

			// When
			got := r.Phase(now)

			// Then
			if got != tt.then {
				t.Errorf("at %s: phase = %s, want %s", tt.now, got, tt.then)
			}
		}
	})

	t.Run("sets available_from to 7 days before start", func(t *testing.T) {
		// Given
		r, _ := event.Parse("abc", query("2026-11-20T19:00:00+01:00", "2026-11-20T21:00:00+01:00"))

		// Then
		if got, want := r.AvailableFrom(), time.Date(2026, 11, 13, 18, 0, 0, 0, time.UTC); !got.Equal(want) {
			t.Errorf("AvailableFrom = %s, want %s", got, want)
		}
	})
}
