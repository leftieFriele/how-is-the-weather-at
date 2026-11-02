package event

import (
	"fmt"
	"net/url"
	"time"

	"how-is-the-weather-at-api/internal/geo"
)

const (
	// From the requirements ("events that start in the next 7 days"), not from met.no, which forecasts about 9 days ahead.
	ForecastWindow = 7 * 24 * time.Hour
	// Keeps forecast lists bounded.
	MaxDuration = 14 * 24 * time.Hour

	maxIDLength = 64
)

type Phase string

const (
	PhaseForecastPending Phase = "forecast_pending" // start is more than 7 days away
	PhaseUpcoming        Phase = "upcoming"         // start is within the next 7 days
	PhaseLive            Phase = "live"             // between start and end
	PhaseEnded           Phase = "ended"            // after end
)

// All times are UTC.
type Request struct {
	ID       string
	Location geo.Point
	Start    time.Time
	End      time.Time
}

type ValidationError struct {
	Param string
	Msg   string
}

func (e *ValidationError) Error() string { return e.Param + ": " + e.Msg }

func invalid(param, format string, args ...any) error {
	return &ValidationError{Param: param, Msg: fmt.Sprintf(format, args...)}
}

func Parse(id string, query url.Values) (Request, error) {
	if !validID(id) {
		return Request{}, invalid("eventId", "must be 1 to %d letters, digits, '-' or '_'", maxIDLength)
	}
	values := make(map[string]string, 4)
	for _, name := range []string{"lat", "lon", "start", "end"} {
		switch v := query[name]; len(v) {
		case 0:
			return Request{}, invalid(name, "is required")
		case 1:
			values[name] = v[0]
		default:
			return Request{}, invalid(name, "must be given once")
		}
	}

	lat, err := geo.ParseLat(values["lat"])
	if err != nil {
		return Request{}, invalid("lat", "%v", err)
	}
	lon, err := geo.ParseLon(values["lon"])
	if err != nil {
		return Request{}, invalid("lon", "%v", err)
	}
	start, err := time.Parse(time.RFC3339, values["start"])
	if err != nil {
		return Request{}, invalid("start", "must be an RFC 3339 time with offset")
	}
	end, err := time.Parse(time.RFC3339, values["end"])
	if err != nil {
		return Request{}, invalid("end", "must be an RFC 3339 time with offset")
	}
	if !end.After(start) {
		return Request{}, invalid("end", "must be after start")
	}
	if end.Sub(start) > MaxDuration {
		return Request{}, invalid("end", "must be at most %d days after start", MaxDuration/(24*time.Hour))
	}

	return Request{
		ID:       id,
		Location: geo.Point{Lat: lat, Lon: lon},
		Start:    start.UTC(),
		End:      end.UTC(),
	}, nil
}

func validID(id string) bool {
	if id == "" || len(id) > maxIDLength {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// AvailableFrom is when the event enters the 7-day forecast window.
func (r Request) AvailableFrom() time.Time { return r.Start.Add(-ForecastWindow) }

func (r Request) Phase(now time.Time) Phase {
	switch {
	case now.Before(r.AvailableFrom()):
		return PhaseForecastPending
	case now.Before(r.Start):
		return PhaseUpcoming
	case now.Before(r.End):
		return PhaseLive
	default:
		return PhaseEnded
	}
}

// Ended events never change, so ok is false for them.
func (r Request) NextPhaseChange(now time.Time) (t time.Time, ok bool) {
	switch r.Phase(now) {
	case PhaseForecastPending:
		return r.AvailableFrom(), true
	case PhaseUpcoming:
		return r.Start, true
	case PhaseLive:
		return r.End, true
	default:
		return time.Time{}, false
	}
}
