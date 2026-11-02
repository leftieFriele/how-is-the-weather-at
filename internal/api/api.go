package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sync/atomic"
	"time"

	"how-is-the-weather-at-api/internal/event"
	"how-is-the-weather-at-api/internal/forecast"
	"how-is-the-weather-at-api/internal/geo"
	"how-is-the-weather-at-api/internal/metno"
)

const (
	attribution = "Based on data from MET Norway (api.met.no), licensed under CC BY 4.0 " +
		"(https://creativecommons.org/licenses/by/4.0/). Values extracted and simplified."
	licenseLink = `<https://creativecommons.org/licenses/by/4.0/>; rel="license"`

	maxStaleWhileRevalidate = 300 * time.Second
	// An ended event's response never changes.
	endedMaxAge = 24 * time.Hour
)

type Forecaster interface {
	Get(ctx context.Context, p geo.Point) (forecast.Data, error)
}

type Server struct {
	forecasts Forecaster
	log       *slog.Logger
	mux       *http.ServeMux
	ready     atomic.Bool
}

func New(forecasts Forecaster, logger *slog.Logger) *Server {
	s := &Server{forecasts: forecasts, log: logger, mux: http.NewServeMux()}
	s.ready.Store(true)
	s.mux.HandleFunc("GET /events/{eventId}/weather", s.weather)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, http.StatusOK, "ok")
	})
	s.mux.HandleFunc("GET /readyz", s.readyz)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// ShuttingDown makes /readyz fail, so the load balancer stops sending traffic.
func (s *Server) ShuttingDown() { s.ready.Store(false) }

// readyz stays healthy during a met.no pause: cached data can still be served. It fails only when shutting down.
func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		writeText(w, http.StatusServiceUnavailable, "shutting down")
		return
	}
	writeText(w, http.StatusOK, "ok")
}

type weatherResponse struct {
	EventID       string         `json:"event_id"`
	Phase         event.Phase    `json:"phase"`
	Location      location       `json:"location"`
	Current       *weatherPoint  `json:"current"`
	Forecast      []weatherPoint `json:"forecast"`
	AvailableFrom *time.Time     `json:"available_from"`
	FetchedAt     *time.Time     `json:"fetched_at"`
	Attribution   string         `json:"attribution"`
}

type location struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type weatherPoint struct {
	Time                  time.Time `json:"time"`
	AirTemperatureCelsius float64   `json:"air_temperature_celsius"`
	WindSpeedMPS          float64   `json:"wind_speed_mps"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (s *Server) weather(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "query: is malformed", 0)
		return
	}
	req, err := event.Parse(r.PathValue("eventId"), query)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), 0)
		return
	}

	now := time.Now()
	resp := weatherResponse{
		EventID:     req.ID,
		Phase:       req.Phase(now),
		Location:    location{Lat: req.Location.Lat.Float(), Lon: req.Location.Lon.Float()},
		Attribution: attribution,
	}

	switch resp.Phase {
	case event.PhaseForecastPending:
		resp.AvailableFrom = new(req.AvailableFrom())
		writeWeather(w, resp, req.AvailableFrom().Sub(now), 0)
	case event.PhaseEnded:
		writeWeather(w, resp, endedMaxAge, 0)
	default:
		data, err := s.forecasts.Get(r.Context(), req.Location)
		if err != nil {
			s.writeUnavailable(w, req, err)
			return
		}
		from := req.Start
		if resp.Phase == event.PhaseLive {
			from = now
		}
		resp.Current = current(data.Steps, now)
		resp.Forecast = points(around(data.Steps, from, req.End))
		resp.FetchedAt = new(data.ValidatedAt.UTC().Truncate(time.Second))

		// A copy may never outlive the 2-hour deadline, and must expire at the next phase change.
		nextPhase, _ := req.NextPhaseChange(now)
		remaining := data.Deadline.Sub(now)
		maxAge := min(remaining, nextPhase.Sub(now), data.FreshFor)
		writeWeather(w, resp, maxAge, min(maxStaleWhileRevalidate, remaining-maxAge))
	}
}

func (s *Server) writeUnavailable(w http.ResponseWriter, req event.Request, err error) {
	u, ok := errors.AsType[*forecast.UnavailableError](err)
	if !ok {
		s.log.Error("unexpected forecast error", slog.String("event_id", req.ID), slog.Any("error", err))
		writeError(w, http.StatusBadGateway, "weather data is unavailable", 0)
		return
	}
	s.log.Info("weather unavailable", slog.String("event_id", req.ID), slog.Int("status", u.Status),
		slog.String("reason", u.Reason))
	writeError(w, u.Status, "weather data is unavailable: "+u.Reason, u.RetryAfter)
}

// around returns the steps inside [from, to] plus the nearest step before and after, so short events between two
// 6-hourly steps still get a forecast. If the event runs past the data, the list ends where the data ends.
func around(steps []metno.Step, from, to time.Time) []metno.Step {
	if len(steps) == 0 {
		return nil
	}
	byTime := func(s metno.Step, t time.Time) int { return s.Time.Compare(t) }
	first, found := slices.BinarySearchFunc(steps, from, byTime)
	if !found {
		first-- // the nearest step before from
	}
	first = max(first, 0)
	last, _ := slices.BinarySearchFunc(steps, to, byTime) // the first step at or after to
	last = min(last, len(steps)-1)
	return steps[first : last+1]
}

// Falls back to the first step if all steps are later than now.
func current(steps []metno.Step, now time.Time) *weatherPoint {
	if len(steps) == 0 {
		return nil
	}
	i, found := slices.BinarySearchFunc(steps, now, func(s metno.Step, t time.Time) int { return s.Time.Compare(t) })
	if !found {
		i = max(i-1, 0)
	}
	return new(point(steps[i]))
}

func points(steps []metno.Step) []weatherPoint {
	out := make([]weatherPoint, 0, len(steps))
	for _, s := range steps {
		out = append(out, point(s))
	}
	return out
}

func point(s metno.Step) weatherPoint {
	return weatherPoint{Time: s.Time.UTC(), AirTemperatureCelsius: s.AirTemperature, WindSpeedMPS: s.WindSpeed}
}

func writeWeather(w http.ResponseWriter, resp weatherResponse, maxAge, staleWhileRevalidate time.Duration) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", fmt.Sprintf("public, max-age=%d, stale-while-revalidate=%d",
		seconds(maxAge), seconds(staleWhileRevalidate)))
	h.Set("Link", licenseLink)
	w.WriteHeader(http.StatusOK)
	_ = json.MarshalWrite(w, resp)
}

func writeError(w http.ResponseWriter, status int, msg string, retryAfter time.Duration) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	if retryAfter > 0 {
		h.Set("Retry-After", fmt.Sprint(int(math.Ceil(retryAfter.Seconds()))))
	}
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, errorResponse{Error: msg})
}

func writeText(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, msg)
}

// seconds rounds down, so a cache lifetime never goes past the limit it was computed from.
func seconds(d time.Duration) int { return max(int(d/time.Second), 0) }
