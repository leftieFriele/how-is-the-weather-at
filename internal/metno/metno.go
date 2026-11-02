// Package metno is a client for the met.no Locationforecast 2.0 compact endpoint.
//
// It follows the met.no Terms of Service (https://api.met.no/doc/TermsOfService): every request identifies us with a
// User-Agent, sends If-Modified-Since with the exact Last-Modified we got before, uses coordinates with at most
// 4 decimals and accepts gzip. Deciding when a request may be sent is up to the caller.
package metno

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"how-is-the-weather-at-api/internal/geo"
)

const DefaultURL = "https://api.met.no/weatherapi/locationforecast/2.0/compact"

const (
	defaultRetryAfter = 60 * time.Second
	defaultExpiresIn  = 30 * time.Minute

	maxBodySize  = 4 << 20
	maxErrorBody = 512
)

type Step struct {
	Time           time.Time
	AirTemperature float64 // degrees Celsius
	WindSpeed      float64 // metres per second
}

type Forecast struct {
	// A 304: the data we have is still the latest, and Steps is empty.
	NotModified bool
	// Ordered by time.
	Steps        []Step
	Expires      time.Time
	LastModified string
	// A 203: the product version will be retired, and we need to migrate.
	Deprecated bool
}

// ThrottledError is a 429. All calls must pause for RetryAfter.
type ThrottledError struct {
	RetryAfter time.Duration
}

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("metno: throttled, retry after %s", e.RetryAfter)
}

// ForbiddenError is a 403: we are blocked, or our identification or coordinates are wrong.
type ForbiddenError struct {
	Body string
}

func (e *ForbiddenError) Error() string { return "metno: forbidden: " + e.Body }

// ClientError is a 4xx other than 403 and 429: our request is wrong, and retrying won't help.
type ClientError struct {
	Status int
	Body   string
}

func (e *ClientError) Error() string { return fmt.Sprintf("metno: status %d: %s", e.Status, e.Body) }

// UpstreamError is a 5xx, an unexpected status, an invalid response, a timeout or a network error.
type UpstreamError struct {
	Status int // 0 if there was no usable response
	Err    error
}

func (e *UpstreamError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("metno: %v", e.Err)
	}
	return fmt.Sprintf("metno: status %d: %v", e.Status, e.Err)
}

func (e *UpstreamError) Unwrap() error { return e.Err }

type Client struct {
	http      *http.Client
	base      *url.URL
	userAgent string
}

// The User-Agent must identify the application and include contact details, as the Terms of Service require.
func NewClient(hc *http.Client, endpoint, userAgent string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("metno: invalid URL %q", endpoint)
	}
	if userAgent == "" {
		return nil, errors.New("metno: a User-Agent is required")
	}
	return &Client{http: hc, base: u, userAgent: userAgent}, nil
}

// A non-empty lastModified is sent as If-Modified-Since. Errors are *ThrottledError, *ForbiddenError, *ClientError or *UpstreamError.
func (c *Client) Get(ctx context.Context, p geo.Point, lastModified string) (Forecast, error) {
	u := c.base.Clone()
	q := u.Query()
	q.Set("lat", p.Lat.String())
	q.Set("lon", p.Lon.String())
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Forecast{}, &UpstreamError{Err: err}
	}
	// Accept-Encoding is left to the transport, which then asks for gzip and decompresses it.
	req.Header.Set("User-Agent", c.userAgent)
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			err = cause
		}
		return Forecast{}, &UpstreamError{Err: err}
	}
	defer func() {
		// Read what is left, so the connection to met.no can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodySize))
		_ = resp.Body.Close()
	}()

	switch status := resp.StatusCode; {
	case status == http.StatusOK || status == http.StatusNonAuthoritativeInfo:
		steps, err := parseCompact(io.LimitReader(resp.Body, maxBodySize))
		if err != nil {
			return Forecast{}, &UpstreamError{Status: status, Err: err}
		}
		return Forecast{
			Steps:        steps,
			Expires:      expires(resp.Header),
			LastModified: resp.Header.Get("Last-Modified"),
			Deprecated:   status == http.StatusNonAuthoritativeInfo,
		}, nil
	case status == http.StatusNotModified:
		return Forecast{
			NotModified:  true,
			Expires:      expires(resp.Header),
			LastModified: cmp.Or(resp.Header.Get("Last-Modified"), lastModified),
		}, nil
	case status == http.StatusTooManyRequests:
		return Forecast{}, &ThrottledError{RetryAfter: retryAfter(resp.Header)}
	case status == http.StatusForbidden:
		return Forecast{}, &ForbiddenError{Body: errorBody(resp.Body)}
	case status >= 400 && status < 500:
		return Forecast{}, &ClientError{Status: status, Body: errorBody(resp.Body)}
	default:
		return Forecast{}, &UpstreamError{Status: status, Err: errors.New(errorBody(resp.Body))}
	}
}

// compact is the part of the compact format we use.
type compact struct {
	Properties struct {
		Timeseries []struct {
			Time time.Time `json:"time"`
			Data struct {
				Instant struct {
					Details struct {
						AirTemperature *float64 `json:"air_temperature"`
						WindSpeed      *float64 `json:"wind_speed"`
					} `json:"details"`
				} `json:"instant"`
			} `json:"data"`
		} `json:"timeseries"`
	} `json:"properties"`
}

func parseCompact(r io.Reader) ([]Step, error) {
	var doc compact
	if err := json.UnmarshalRead(r, &doc); err != nil {
		return nil, fmt.Errorf("invalid forecast: %w", err)
	}
	steps := make([]Step, 0, len(doc.Properties.Timeseries))
	for _, ts := range doc.Properties.Timeseries {
		d := ts.Data.Instant.Details
		if d.AirTemperature == nil || d.WindSpeed == nil {
			continue
		}
		steps = append(steps, Step{Time: ts.Time.UTC(), AirTemperature: *d.AirTemperature, WindSpeed: *d.WindSpeed})
	}
	if len(steps) == 0 {
		return nil, errors.New("invalid forecast: no time steps")
	}
	slices.SortFunc(steps, func(a, b Step) int { return a.Time.Compare(b.Time) })
	return steps, nil
}

func expires(h http.Header) time.Time {
	if t, err := http.ParseTime(h.Get("Expires")); err == nil {
		return t
	}
	return time.Now().Add(defaultExpiresIn)
}

func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return defaultRetryAfter
}

func errorBody(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, maxErrorBody))
	return string(b)
}
