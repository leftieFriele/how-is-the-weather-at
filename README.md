# How is the weather at API

API for returning the weather for an event location, using the [Met.no API Location Forecast API](https://api.met.no/weatherapi/locationforecast/2.0/documentation) 

## How to build
Requirements: Go (version in `go.mod`), and optionally Docker.

```sh
make build                                      # puts how-is-the-weather-at-api and fakemetno in bin/
go build -o bin/ ./cmd/...                      # the same without make
docker build -t how-is-the-weather-at-api .     # container image (or: make docker)
```

The image is a multi-stage build with a static binary on a distroless base, running as a non-root user.

## How to test
```sh
make test                                       # all tests, with the race detector (go test -race ./...)
go test -v ./internal/forecast                  # read the behaviours as a specification
go test ./internal/forecast -run 'TestForecastCache/never_serves_data_after'   # a single behaviour
go test -cover ./...                            # coverage
make lint                                       # golangci-lint run
```

- Tests are written as behaviours: one test function per feature, one subtest per behaviour, with a sentence as its name and
  `// Given`, `// When`, `// Then` steps. `go test -v` reads like a specification.
- They go deepest on the caching rules (`TestForecastCache`) and the rate limiting (`TestUpstreamBudget`, `TestRateLimiting`).
  100% coverage is not a goal.
- Tests never call the real met.no and never sleep. Time-dependent tests run in `testing/synctest` on fake time, against the
  fake met.no in memory. The end-to-end test (`TestEndToEnd`) runs the whole service over real HTTP and checks every response
  against `openapi.yaml`.

## How to run
The code has two programs:
- `cmd/how-is-the-weather-at-api`: the service.
- `cmd/fakemetno`: a fake met.no for development and tests, so they never call the real API. It returns forecasts in met.no's
  compact format (hourly for 60 hours, then 6-hourly up to 9 days), with `Expires` 30 minutes ahead and `Last-Modified`.
  It answers `If-Modified-Since` with `304`, and it returns `403` like met.no if the User-Agent is missing or a coordinate has
  more than 4 decimals, so ToS mistakes show up locally.

Requirements are the same as for [building](#how-to-build). Docker is only needed for `docker compose`.

### Against the fake met.no (recommended)
```sh
make run-fake
```
This starts the fake met.no on port 8081 and the service on port 8080. Without `make`:

```sh
go run ./cmd/fakemetno -port 8081 &
go run ./cmd/how-is-the-weather-at-api \
  -upstream-url http://localhost:8081/weatherapi/locationforecast/2.0/compact \
  -user-agent "how-is-the-weather-at-api-dev local"
```

### With Docker
```sh
docker compose up
```
This runs the service and the fake met.no in containers, on the same ports.

### Against the real met.no
```sh
make run USER_AGENT="how-is-the-weather-at-api-dev/0.1 you@example.com"
# or
go run ./cmd/how-is-the-weather-at-api -user-agent "how-is-the-weather-at-api-dev/0.1 you@example.com" -upstream-rate 1
```

**Why the User-Agent is required.** The [met.no Terms of Service](https://api.met.no/doc/TermsOfService) require every
request to identify the application with a User-Agent that has its name and contact details (an email or website), such as
`how-is-the-weather-at-api/1.0 you@example.com`, so met.no knows who sends the traffic and can contact them. Requests without it get `403 Forbidden`, and fake or random identification
can get the application banned permanently. There is no default, because no single value would carry the right contact
details for everyone running the service. So both `-user-agent` and `make run` refuse to start without one, and nobody can
call met.no anonymously by accident. The fake met.no doesn't need real contact details, which is why `make run-fake` sets a
local value itself.

Keep `-upstream-rate` low when running against the real met.no: the 20 req/s limit is shared with every other environment
using the same identification.

### Trying it
```sh
curl -i http://localhost:8080/healthz
curl -i -G http://localhost:8080/events/abc123/weather \
  --data-urlencode "lat=59.9139" --data-urlencode "lon=10.7522" \
  --data-urlencode "start=2026-11-05T18:00:00+01:00" --data-urlencode "end=2026-11-05T23:00:00+01:00"
```
Change `start` and `end` to times within the next 7 days. More examples are in [Example usage](#example-usage).

Stop the service with Ctrl-C. It stops taking requests, lets running ones finish, then cancels met.no calls and exits.

## Example usage
The examples assume the service runs locally on port 8080 (see [How to run](#how-to-run)) and that the time is `2026-11-02T09:00:00Z`.

Use `-G --data-urlencode` so curl encodes the query string. A `+` in a time offset must be sent as `%2B`, otherwise it is
read as a space and the request gets `400`.

### Upcoming event
An event in Oslo on 5 November, 18:00–23:00 Norwegian time. It is more than 2.5 days ahead, so met.no only has 6-hourly steps:
18:00Z is inside the event, and 12:00Z and 00:00Z are the nearest steps before and after it.

```sh
curl -i -G http://localhost:8080/events/abc123/weather \
  --data-urlencode "lat=59.9139" \
  --data-urlencode "lon=10.7522" \
  --data-urlencode "start=2026-11-05T18:00:00+01:00" \
  --data-urlencode "end=2026-11-05T23:00:00+01:00"
```

```
HTTP/1.1 200 OK
Content-Type: application/json
Cache-Control: public, max-age=1200, stale-while-revalidate=300
Link: <https://creativecommons.org/licenses/by/4.0/>; rel="license"
```
```json
{
  "event_id": "abc123",
  "phase": "upcoming",
  "location": { "lat": 59.9139, "lon": 10.7522 },
  "current":  { "time": "2026-11-02T09:00:00Z", "air_temperature_celsius": 4.2, "wind_speed_mps": 3.1 },
  "forecast": [
    { "time": "2026-11-05T12:00:00Z", "air_temperature_celsius": 2.1, "wind_speed_mps": 4.8 },
    { "time": "2026-11-05T18:00:00Z", "air_temperature_celsius": 1.0, "wind_speed_mps": 5.4 },
    { "time": "2026-11-06T00:00:00Z", "air_temperature_celsius": -0.6, "wind_speed_mps": 5.9 }
  ],
  "available_from": null,
  "fetched_at": "2026-11-02T08:47:12Z",
  "attribution": "Based on data from MET Norway (api.met.no), licensed under CC BY 4.0 (https://creativecommons.org/licenses/by/4.0/). Values extracted and simplified."
}
```

`max-age=1200` is the time until met.no's `Expires` (09:20:00Z). The data's `deadline` is 10:47:12Z, two hours after `fetched_at`,
so `max-age + stale-while-revalidate` stays well within it.

### Event more than 7 days away
No met.no request is made. `available_from` is `start − 7 days`, and the response can be cached until then.

```sh
curl -i -G http://localhost:8080/events/def456/weather \
  --data-urlencode "lat=60.3913" \
  --data-urlencode "lon=5.3221" \
  --data-urlencode "start=2026-11-20T18:00:00Z" \
  --data-urlencode "end=2026-11-20T20:00:00Z"
```

```
HTTP/1.1 200 OK
Content-Type: application/json
Cache-Control: public, max-age=982800, stale-while-revalidate=0
```
```json
{
  "event_id": "def456",
  "phase": "forecast_pending",
  "location": { "lat": 60.3913, "lon": 5.3221 },
  "current": null,
  "forecast": [],
  "available_from": "2026-11-13T18:00:00Z",
  "fetched_at": null,
  "attribution": "Based on data from MET Norway (api.met.no), licensed under CC BY 4.0 (https://creativecommons.org/licenses/by/4.0/). Values extracted and simplified."
}
```

### Ended event
No met.no request is made. The coordinates are sent with 6 decimals and truncated to 4 in `location`.

```sh
curl -i -G http://localhost:8080/events/ghi789/weather \
  --data-urlencode "lat=63.430512" \
  --data-urlencode "lon=10.395053" \
  --data-urlencode "start=2026-10-30T17:00:00Z" \
  --data-urlencode "end=2026-10-30T19:00:00Z"
```

```
HTTP/1.1 200 OK
Content-Type: application/json
Cache-Control: public, max-age=86400, stale-while-revalidate=0
```
```json
{
  "event_id": "ghi789",
  "phase": "ended",
  "location": { "lat": 63.4305, "lon": 10.395 },
  "current": null,
  "forecast": [],
  "available_from": null,
  "fetched_at": null,
  "attribution": "Based on data from MET Norway (api.met.no), licensed under CC BY 4.0 (https://creativecommons.org/licenses/by/4.0/). Values extracted and simplified."
}
```

### Invalid input

```sh
curl -i -G http://localhost:8080/events/abc123/weather \
  --data-urlencode "lat=5.99e1" \
  --data-urlencode "lon=10.7522" \
  --data-urlencode "start=2026-11-05T18:00:00+01:00" \
  --data-urlencode "end=2026-11-05T23:00:00+01:00"
```

```
HTTP/1.1 400 Bad Request
Content-Type: application/json
Cache-Control: no-store
```
```json
{ "error": "lat: must be a plain decimal number" }
```

## Architecture

```
Phone app ──HTTPS──▶ how-is-the-weather-at-api (single Go instance) ──HTTPS──▶ api.met.no
                     ├─ request validation
                     ├─ forecast cache       (LRU, location → forecast + Expires/Last-Modified/validated_at)
                     └─ upstream client      (User-Agent, conditional GET, rate limits, backoff and pauses)
```

- **Our service is the proxy the met.no ToS asks for.** Phones never call met.no directly.
- **The phone app is ours, but the server does not trust it.** All input comes from the client and is validated. Every response depends only
  on the request URL, the current time and met.no's data, so one client can't change what another client sees.
- **Forecasts are cached by location (coordinates truncated to 4 decimals), not by eventId.** All users of an event send the same coordinates,
  so the cache holds about one entry per event, and events at the same venue share one entry.
- **Freshness comes from met.no's `Expires` header, capped at 2 hours.** We never call met.no for a location before `Expires`.
  After that, we send a conditional request with `If-Modified-Since` set to the stored `Last-Modified`. Users never see data more than
  2 hours after met.no last confirmed it (see [Freshness](#freshness)). Memory is bounded by the LRU size limit, with no cleanup job.
- **Upstream traffic is capped at half of met.no's 20 req/s per application** by one shared budget. Known locations go first,
  new locations get what is left, so abuse can only delay brand-new events (see [Upstream budget](#upstream-budget)).
  Concurrent requests for the same location share one upstream call. Errors from met.no pause or slow down our calls
  (see [Talking to met.no](#talking-to-metno)).
- **Configuration** uses command-line flags, with one exception for the port. A `-user-agent` with contact info is required by the ToS,
  and the service refuses to start without it. `-upstream-rate` (default 10 req/s) must be at most 20, or the service refuses to start.
  The listening port comes from `-port` if given, otherwise from the `PORT` environment variable, otherwise 8080. Many container
  platforms set `PORT`, so the service runs there without extra setup. `-cache-size` (default 10 000) is the most locations kept
  in the cache.

## Request flow
1. **Validate the input** (see [Input parameters](#input-parameters)). Invalid requests get `400`.
2. **Decide the event's phase** from `start`, `end` and the current time (see [API response](#api-response)).
3. **`forecast_pending` and `ended` respond right away**, with no weather data and no cache lookup or met.no request.
4. **`upcoming` and `live` look up the location in the cache** (see [Freshness](#freshness)), fetching from met.no if needed.

## Freshness
Forecasts shown to users must be no more than 2 hours old. We measure age from the last time met.no confirmed the data:

```
validated_at = time of the last 200 or 304 from met.no for this location
deadline     = validated_at + 2h      ← the data is never shown to anyone after this
```

A `304` from met.no means our copy is still the latest, so it resets `validated_at` and replaces the stored `Expires`.
We don't use met.no's model run time (`meta.updated_at`), because we can't control it and the ToS forbids polling for it.

Cache state of a location at time `now` (only looked at for `upcoming` and `live` events):

| State | When | What we do |
|---|---|---|
| Fresh | `now < Expires` | Serve from cache. No met.no request. |
| Refreshing | `Expires ≤ now < deadline` | Serve from cache right away, and start one background refresh. |
| Past deadline | `now ≥ deadline` | Treated as not cached. The request waits for a refresh (with a timeout). |
| Missing | not in cache | The request waits for a fetch (with a timeout). |

- **Failed background refresh** (5xx, timeout, network error): we keep serving until `deadline` and retry with per-location backoff
  (see [Talking to met.no](#talking-to-metno)). The backoff is capped so at least one retry happens before `deadline`.
- **Failed refresh while the request waits**: `502`, or `503` if the upstream budget is used up or met.no asked us to back off.
  A request waits at most 3 seconds and then gets `503` with `Retry-After: 1`; the met.no call keeps running so the next request
  can use the result.
- **`Expires` later than `deadline`**: the ToS wins. We don't call met.no before `Expires`, and we don't serve the data after
  `deadline`, so in that gap the location is unavailable (`503`). This should almost never happen; it is logged and counted.
- **A met.no `429` pause longer than an entry's remaining time**: the entry passes its deadline and returns `503`.

The 2-hour limit also covers copies kept by the phone or any cache in front of us, so the response headers never let a copy live past
`deadline` (see [Response headers](#response-headers)). The app is expected to request the weather when the event screen opens, and
again if the screen stays open past `max-age`. On `503` it should retry after `Retry-After`, plus a small random delay so phones
don't all retry in the same second.

## Talking to met.no
Every request to met.no:
- goes to `https://api.met.no/weatherapi/locationforecast/2.0/compact`. Plain `http` is only allowed for the `-upstream-url` flag, so
  a local fake met.no can be used for development and tests.
- sends the `-user-agent` value, and `If-Modified-Since` when we have a `Last-Modified`.
- accepts gzip. Go's HTTP client asks for gzip and unpacks it automatically, but only if we don't set `Accept-Encoding` ourselves.
  A test against the fake met.no checks that the header is sent.
- has coordinates with at most 4 decimals, because they are built from our integer coordinates (see [Input parameters](#input-parameters)).
- has a 10s timeout.

How we react to met.no's answers:

| Answer | Scope | What we do |
|---|---|---|
| `200`, `304` | location | Store the data, `Last-Modified` and `Expires`, and set `validated_at`. Reset the location's backoff and the global failure count. |
| `203` | — | Use the data as normal. The product version is deprecated, so we log a warning (at most once per hour) and count it. Alert on it: we need to migrate. |
| `429` | all calls | Pause all calls for `Retry-After` (60s if missing). Log a warning and count it. |
| `403` | all calls | Pause all calls for 15 min, then send one probe request. Another `403` doubles the pause, up to 1h. Log an error with the status and the start of the response body, and alert. A human must look at it: it means we are blocked or our identification is wrong. |
| Other `4xx` | location | Our request is wrong. Log an error and back off the location for the maximum time (5 min). |
| `5xx`, timeout, network error | location + global | Back off the location (30s, doubling to 5 min, with random jitter). After 5 failures in a row across all locations, pause all calls for 30s, doubling to 5 min. The first success resets it. |

- **A location in backoff gets no met.no calls.** Until the backoff ends, requests for it get cached data younger than 2 hours if we have
  it, and otherwise `502` with `Retry-After` at once. Without this, a failing location would be fetched again on every request, since
  shared upstream calls only cover requests that overlap in time.
- **During a pause** (`429`, `403` or the global failure pause), requests get cached data younger than 2 hours if we have it, and
  otherwise `503` with `Retry-After` set to the remaining pause.
- **`/readyz` stays healthy during a pause.** Failing it would take the instance out of traffic, and then not even cached data could be served.

### Upstream budget
All met.no calls share one budget: a token bucket that refills at `-upstream-rate` (default 10/s) and holds at most 5 tokens.
Each call takes one token. There are two kinds of calls:

| Kind | Examples | Rule |
|---|---|---|
| Known location | background refresh, entry past its deadline | May take any token. |
| New location | first request for a location | May only take a token while more than 2 are left. |

- **Known locations always go first.** Spamming new coordinates can never take the last 2 tokens, so existing events keep being
  refreshed. After a restart nothing is known, so new locations get nearly the whole budget.
- **No waiting.** A background refresh without a token is skipped; the next request for that location tries again. A request that
  needs a met.no call and gets no token fails at once with `503` and `Retry-After: 1`. Requests for the same location share one
  call, so many users opening the same new event use one token.
- **Why 10/s and not 20/s:** the limit is per application, across all our environments. During a deploy, the old and new instance
  overlap briefly and add up to at most 20/s. Staging and local development use the same identification, so they should run with a
  much lower `-upstream-rate`, e.g. 1.
- **Why 5 tokens:** a full bucket allows a burst. With 5 tokens, the worst case in any second is 5 + 10 = 15 calls, safely under 20.
- **Jitter:** a location may refresh at `Expires` plus a random 0–60s. met.no often gives many locations the same `Expires`, and
  the ToS asks us not to send synchronised requests. This only delays calls, so we still never call before `Expires`.
- **Capacity limit:** if known locations alone need the whole budget, new events get `503`. With a typical `Expires` of 30–60 min,
  10/s keeps about 18 000–36 000 events viewed at the same time fresh. Beyond that we would need a special agreement with met.no.
  Budget usage is alerted on well before that point.

## Input parameters
The client sends the event's identifier (eventId), its location and its start and end time. None of it is trusted.
Weather data is cached by location. The eventId is validated, echoed in the response and used in logs. It is in the path so that
events can later be looked up in an authoritative source without changing the API (see [Further work](#further-work--considerations)).

```
GET /events/{eventId}/weather?lat=59.9139&lon=10.7522&start=2026-11-05T18:00:00+01:00&end=2026-11-05T23:00:00+01:00
```

| Parameter | Where | Rules |
|---|---|---|
| `eventId` | path | Letters, digits, `-` and `_`, max 64 characters. |
| `lat` | query | Plain decimal number, -90 to 90. Truncated to 4 decimals. |
| `lon` | query | Plain decimal number, -180 to 180. Truncated to 4 decimals. |
| `start` | query | Event start, RFC 3339 with offset. |
| `end` | query | Event end, RFC 3339 with offset. Must be after `start`. Max event length: 14 days, to keep forecast lists bounded. |

The request is rejected with **400 Bad Request** if:
- a parameter is missing, malformed, out of range or duplicated
- a number is written as `NaN`/`Inf`, in hex or with an exponent

**Coordinates are truncated on the server**, so the ToS limit of 4 decimals doesn't depend on every app version getting it right:
- We truncate the text, not a float: after validating the number, digits after the 4th decimal are dropped. Going through a float
  first could turn `59.9139` into `59.91389999…` and truncate it to `59.9138`.
- Negative values are truncated towards zero: `-33.86789` becomes `-33.8678`.
- Coordinates are stored as integers in units of 1/10 000 degree. `10.75`, `10.7500` and `10.750012` are the same cache key, and the
  met.no URL is formatted from the integers.
- The response's `location` shows the truncated coordinates that were used.

## API response
The response is always JSON with the same structure, giving air temperature in Celsius and wind speed in m/s.
Its content depends on the event's phase at the time of the request:

| Phase | When | `current` | `forecast` |
|---|---|---|---|
| `forecast_pending` | `start` is more than 7 days away | `null` | `[]`, with `available_from` set to `start − 7 days` |
| `upcoming` | `start` is within the next 7 days | weather at the venue right now | entries around the event (see below) |
| `live` | between `start` and `end` | weather right now | entries from now until `end` |
| `ended` | after `end` | `null` | `[]` |

The app only needs to look at `phase`. The 7-day window comes from the requirements ("events that start in the next 7 days"), not
from met.no, which forecasts about 9 days ahead. It is a constant, not a flag, so it can't be raised past what met.no covers.
The phase is decided from `start`, `end` and the current time alone, before any met.no request.
`live` and `ended` go beyond the requirements: users also open the event screen during and after an event.

```json
{
  "event_id": "abc123",
  "phase": "upcoming",
  "location": { "lat": 59.9139, "lon": 10.7522 },
  "current":  { "time": "2026-11-02T09:00:00Z", "air_temperature_celsius": 4.2, "wind_speed_mps": 3.1 },
  "forecast": [
    { "time": "2026-11-05T17:00:00Z", "air_temperature_celsius": 1.0, "wind_speed_mps": 5.4 }
  ],
  "available_from": null,
  "fetched_at": "2026-11-02T08:47:12Z",
  "attribution": "Based on data from MET Norway (api.met.no), licensed under CC BY 4.0 (https://creativecommons.org/licenses/by/4.0/). Values extracted and simplified."
}
```

- All times are UTC.
- `fetched_at` is `validated_at`: when met.no last confirmed the data. It is `null` when no met.no data was needed.
- met.no forecasts are hourly for about the first 2.5 days and 6-hourly (00, 06, 12, 18 UTC) after that. Each entry is the forecast
  for that exact time, so we include the forecast steps inside the event plus the nearest step before and after it.
  An hourly 18:00–20:00 event gets 18, 19 and 20. A 6-hourly 19:00–21:00 event gets 18 and 00, so it still gets a forecast.
  A `live` event uses the same rule with now in place of `start`. If the event runs past met.no's data, the list ends where the data ends.
- `ended` and `forecast_pending` events never trigger a met.no request.

### Response headers
- `Cache-Control: public, max-age=N, stale-while-revalidate=S`, where
  ```
  remaining = deadline − now
  N = min(remaining, time until the event's next phase change, F)
      F = time until met.no's Expires   (Fresh)
        = 60s                           (Refreshing: a refresh is running, so the next request gets new data)
  S = min(300s, remaining − N)          so N + S ≤ remaining, always
  ```
  The phase cap makes the app switch phase on time. For `forecast_pending` there is no met.no data and no `deadline`, so N is
  the time until `available_from` and S is 0. An `ended` response never changes, so N is 1 day and S is 0. There is no `stale-if-error`, since that would let a copy outlive `deadline`.
- No `Expires`, `Last-Modified` or `ETag` header, and no `304` responses. Our response depends on the current time as well as met.no's
  data (`current`, `phase`, and a `live` forecast that shrinks over time), so met.no's `Last-Modified` can't validate it. Responses are
  about 1 KB, so conditional requests would save little. `max-age` is relative, so it doesn't depend on the phone's clock.
- `Link: <https://creativecommons.org/licenses/by/4.0/>; rel="license"`. Together with the `attribution` field, this meets the CC BY 4.0 credit requirement.

### Status codes
| Code | Meaning |
|---|---|
| 200 | Weather returned. `forecast_pending` and `ended` events return 200 with `current: null` and `forecast: []`. |
| 400 | Invalid input (see Input parameters). |
| 502 | No data younger than 2 hours, and met.no failed for this location (now or recently, see [Talking to met.no](#talking-to-metno)). Includes `Retry-After` when the location is in backoff. |
| 503 | No data younger than 2 hours, and no upstream budget token was free, all met.no calls are paused (`429`, `403` or repeated failures), the ToS forbids calling met.no yet, or the met.no call didn't finish within 3 seconds. Includes `Retry-After`. |

Cached data younger than 2 hours is served instead of a 502 or 503, even after met.no's `Expires`. Older data is never served.
Error responses have `Cache-Control: no-store` and a JSON body with a single `error` message:

```json
{ "error": "lat: must be a plain decimal number" }
```

### Health checks
- `GET /healthz` answers `200` while the process runs.
- `GET /readyz` answers `200`, and `503` once shutdown has started, so a load balancer stops sending traffic. It stays `200`
  while met.no calls are paused, because cached data can still be served.

## OpenAPI specification
The API is described in [`openapi.yaml`](openapi.yaml), which can be used to generate clients. It uses OpenAPI 3.0.3 rather than
3.1, because client generators support 3.0 better. For example, a Kotlin client:

```sh
npx @openapitools/openapi-generator-cli generate -i openapi.yaml -g kotlin -o clients/kotlin
```

- `lat` and `lon` are strings in the spec, not numbers. The server only accepts plain decimal numbers, and many languages
  format small floating-point numbers with an exponent (e.g. `5e-05`), which would be rejected.
- Check the spec with `npx @redocly/cli lint openapi.yaml`.

## Further work & considerations

Deliberately left out for now:
- **Shared cache and multiple instances.** We run a single instance with an in-memory cache, which is enough for the expected peak of ~80 req/s.
  Running several instances would need a shared cache (e.g. Redis) and a *shared* rate limit, so the combined traffic stays under met.no's 20 req/s.
- **Trusted event data.** Events come from the client unverified. The long-term fix is to look events up in an authoritative source, or to have
  the system that hands events to the app sign them so we can verify the signature.
- **Per-client rate limiting.** Skipped for now. Phones often share IP addresses behind carrier NAT, so a per-IP limit must be generous.
- **A caching layer/CDN in front.** Our headers already support one. URLs that differ only in extra coordinate decimals are cached
  separately there; our own cache key is not affected. Not set up yet.
- **Raw lat/lon endpoint.** Removed in favour of events-only, to keep the attack surface small.
- **Background prefetching before predictable bursts.** Not done, because it amounts to polling met.no. If it's ever added, requests must be spread out with random delays as the ToS requires.
- **Configuration via env vars or config files.** Flags only, by choice, except `PORT` (see [Architecture](#architecture)).
- **A per-event location limit** (e.g. at most 5 locations per eventId, otherwise `409`). Considered and dropped: it can't stop abuse
  while eventIds are unverified, since an attacker just uses a new eventId, and it turns app bugs into user-facing errors. A bug such as
  sending the phone's GPS position instead of the event's location shows up as a rising new-location rate and a falling cache hit rate.

Open points and possible improvements:
- Finalise the eventId format and the maximum event length (14 days is a placeholder).
- Send `altitude` to met.no for more accurate temperatures. This needs a trusted source for venue altitude.
- More weather data (precipitation, weather symbol, uncertainty ranges from the `/complete` endpoint).
- Keep the cache across restarts. A restart currently starts empty, so at peak many first requests get `503` until the cache
  has warmed up (about 2 000 locations per 3½ minutes at 10/s).
- Metrics: the service logs the events listed in [Talking to met.no](#talking-to-metno), but has no metrics yet. Next would be
  cache hit rate, new-location rate, upstream budget usage, 502/503 counts, met.no status codes, pause and backoff counts.
- Use 3 decimals (~110 m) for the cache key instead of 4 (~11 m). met.no's model grid is about 1 km, so nearby venues could share an
  entry and a met.no call without losing accuracy. Do it once we can measure the cache hit rate.
