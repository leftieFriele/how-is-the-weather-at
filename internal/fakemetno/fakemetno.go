// Package fakemetno is a fake met.no Locationforecast compact endpoint, for local development and tests.
//
// It generates forecasts from the current time, sends Expires and Last-Modified, answers If-Modified-Since with 304,
// and, like met.no, returns 403 when the User-Agent is missing or a coordinate has more than 4 decimals.
// Tests can make it fail or slow down, and inspect the requests it got.
package fakemetno

import (
	"compress/gzip"
	"context"
	"encoding/json/v2"
	"io"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

const DefaultExpiresIn = 30 * time.Minute

type Request struct {
	Time            time.Time
	Lat             string
	Lon             string
	UserAgent       string
	IfModifiedSince string
	AcceptEncoding  string
}

// Server's zero value is not usable; use New.
type Server struct {
	mu        sync.Mutex
	requests  []Request
	expiresIn time.Duration
	delay     time.Duration
	status    int
	header    http.Header
	modelRun  time.Time
}

func New() *Server {
	return &Server{expiresIn: DefaultExpiresIn, modelRun: time.Now().UTC().Truncate(time.Second)}
}

func (s *Server) SetExpiresIn(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expiresIn = d
}

func (s *Server) SetDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = d
}

// SetStatus answers valid requests with status and header instead of a forecast. 0 goes back to forecasts.
func (s *Server) SetStatus(status int, header http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.header = status, header
}

// NewModelRun sets Last-Modified to now, as if met.no had published a new forecast.
func (s *Server) NewModelRun() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelRun = time.Now().UTC().Truncate(time.Second)
}

func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Client calls the fake in memory, without a network connection, so inside a synctest test the fake runs on
// the test's fake time.
func (s *Server) Client() *http.Client {
	return &http.Client{Transport: transport{s}}
}

type transport struct{ h http.Handler }

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		t.h.ServeHTTP(rec, req)
	}()
	select {
	case <-done:
		return rec.Result(), nil
	case <-req.Context().Done():
		<-done // the handler stops on cancellation too
		return nil, context.Cause(req.Context())
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lat, lon := q.Get("lat"), q.Get("lon")
	s.mu.Lock()
	s.requests = append(s.requests, Request{
		Time:            time.Now(),
		Lat:             lat,
		Lon:             lon,
		UserAgent:       r.Header.Get("User-Agent"),
		IfModifiedSince: r.Header.Get("If-Modified-Since"),
		AcceptEncoding:  r.Header.Get("Accept-Encoding"),
	})
	delay, status, header := s.delay, s.status, s.header
	expires := time.Now().Add(s.expiresIn)
	lastModified := s.modelRun.Format(http.TimeFormat)
	s.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}

	// The same checks met.no makes.
	if r.Header.Get("User-Agent") == "" {
		http.Error(w, "missing User-Agent identification", http.StatusForbidden)
		return
	}
	latF, latOK := parseCoord(lat)
	lonF, lonOK := parseCoord(lon)
	if !latOK || !lonOK {
		http.Error(w, "lat and lon must be numbers with at most 4 decimals", http.StatusForbidden)
		return
	}

	if status != 0 {
		maps.Copy(w.Header(), header)
		http.Error(w, http.StatusText(status), status)
		return
	}

	w.Header().Set("Expires", expires.UTC().Format(http.TimeFormat))
	w.Header().Set("Last-Modified", lastModified)
	if r.Header.Get("If-Modified-Since") == lastModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	var out io.Writer = w
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer func() { _ = gz.Close() }()
		out = gz
	}
	_ = json.MarshalWrite(out, forecast(time.Now(), latF, lonF))
}

func parseCoord(s string) (float64, bool) {
	if _, frac, ok := strings.Cut(s, "."); ok && len(frac) > 4 {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

type compact struct {
	Type       string `json:"type"`
	Properties struct {
		Timeseries []timestep `json:"timeseries"`
	} `json:"properties"`
}

type timestep struct {
	Time time.Time `json:"time"`
	Data struct {
		Instant struct {
			Details struct {
				AirTemperature float64 `json:"air_temperature"`
				WindSpeed      float64 `json:"wind_speed"`
			} `json:"details"`
		} `json:"instant"`
	} `json:"data"`
}

// forecast generates met.no's time steps from now: hourly for 60 hours, then 6-hourly (00, 06, 12, 18 UTC)
// up to 9 days ahead. The values follow a daily cycle and depend on the location, so they look plausible.
func forecast(now time.Time, lat, lon float64) compact {
	var doc compact
	doc.Type = "Feature"
	start := now.UTC().Truncate(time.Hour)
	end := start.Add(9 * 24 * time.Hour)
	for t := start; !t.After(end); {
		var ts timestep
		ts.Time = t
		hour := float64(t.Hour()) + lon/15
		d := &ts.Data.Instant.Details
		d.AirTemperature = math.Round((15-lat/4+4*math.Sin((hour-9)*math.Pi/12))*10) / 10
		d.WindSpeed = math.Round((4+2*math.Cos(hour*math.Pi/12)+math.Mod(math.Abs(lat+lon), 3))*10) / 10
		doc.Properties.Timeseries = append(doc.Properties.Timeseries, ts)

		if t.Sub(start) < 59*time.Hour {
			t = t.Add(time.Hour)
		} else {
			t = t.Truncate(6 * time.Hour).Add(6 * time.Hour)
		}
	}
	return doc
}
