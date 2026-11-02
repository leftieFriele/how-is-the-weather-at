# How is the weather at API

API for returning the weather for an event location, using met.no's
[Locationforecast API](https://api.met.no/weatherapi/locationforecast/2.0/documentation).

## How to build
Requirements: Go (version in `go.mod`). Docker is optional: for the container image, `docker compose` and a local Redis.

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
go test ./internal/forecast -run 'TestForecastCache/never_serves_data_after_the_2-hour_deadline,_even'   # one behaviour
go test -cover ./...                            # coverage
make lint                                       # golangci-lint run
```

- Tests are written as behaviours: one test function per feature, one subtest per behaviour, with a sentence as its name and
  `// Given`, `// When`, `// Then` steps. `go test -v` reads like a specification.
- They go deepest on the caching rules (`TestForecastCache`) and the rate limiting (`TestUpstreamBudget`, `TestRateLimiting`).
  100% coverage is not a goal.
- Tests never call the real met.no and never sleep: time-dependent tests run on fake time with `testing/synctest`.
- The caching and rate-limiting behaviours run against both versions of the [shared state](#shared-state), as the subtests
  `in_memory` and `redis`. The Redis one uses [miniredis](https://github.com/alicebob/miniredis), an in-memory Redis.
- `TestInstances` runs three instances sharing one Redis under heavy traffic, and checks that met.no never gets more than 20 calls
  in a second, one call per location, and one probe after a `403`.
- The end-to-end tests run the whole service over real HTTP, with miniredis as Redis: `TestEndToEnd` checks every response
  against `openapi.yaml`, and `TestFailClosed` checks what happens when Redis goes down.

## How to run
The code has two programs:
- `cmd/how-is-the-weather-at-api`: the service.
- `cmd/fakemetno`: a fake met.no for development and tests, so they never call the real API. It returns forecasts in met.no's
  compact format with met.no's time steps, `Expires` 30 minutes ahead and `Last-Modified`. It answers `If-Modified-Since` with
  `304`, and returns `403` like met.no if the User-Agent is missing or a coordinate has more than 4 decimals, so ToS mistakes
  show up locally.

### Configuration
The service is configured with flags only, except the port, which many container platforms set through `PORT`.

| Flag | Default | Rule |
|---|---|---|
| `-user-agent` | — | Required: application name and contact details (see [below](#why-the-user-agent-is-required)). |
| `-redis-url` / `-in-memory` | — | Exactly one. `-redis-url` shares the state between any number of instances; `-in-memory` is for one instance, in tests and local runs (see [Shared state](#shared-state)). |
| `-upstream-rate` | 10 | met.no calls per second, at most 15 (see [Upstream budget](#upstream-budget)). |
| `-upstream-url` | met.no's compact endpoint | Point it at the fake met.no for local runs. |
| `-port` | `$PORT`, then 8080 | |
| `-cache-size` | 10 000 | Locations kept with `-in-memory`. |

### Against the fake met.no (recommended)
```sh
make run-fake
```
This starts the fake met.no on port 8081 and one in-memory instance of the service on port 8080. Without `make`:

```sh
go run ./cmd/fakemetno -port 8081 &
go run ./cmd/how-is-the-weather-at-api -in-memory \
  -upstream-url http://localhost:8081/weatherapi/locationforecast/2.0/compact \
  -user-agent "how-is-the-weather-at-api-dev local"
```

### With Docker, several instances
```sh
docker compose up --scale api=3
```
This runs Redis, the fake met.no, three instances of the service sharing Redis, and nginx in front of them on port 8080.
The instances don't publish ports themselves, so any number of them can run.

### Against the real met.no
```sh
make run USER_AGENT="how-is-the-weather-at-api-dev/0.1 you@example.com"   # one in-memory instance at -upstream-rate 1
```
For shared state, start a local Redis and use `-redis-url` instead of `-in-memory`:

```sh
docker run -d -p 6379:6379 redis:8-alpine
go run ./cmd/how-is-the-weather-at-api -redis-url redis://localhost:6379 -upstream-rate 1 \
  -user-agent "how-is-the-weather-at-api-dev/0.1 you@example.com"
```
Keep `-upstream-rate` low: local runs add to production's share of met.no's limit (see [Upstream budget](#upstream-budget)).

#### Why the User-Agent is required
The [met.no Terms of Service](https://api.met.no/doc/TermsOfService) require every request to carry a User-Agent with the
application's name and contact details, e.g. `how-is-the-weather-at-api/1.0 you@example.com`, so met.no knows who sends the
traffic. Without one met.no answers `403`, and fake identification can get the application banned. No default would have the
right contact details for everyone running the service, so the service and `make run` refuse to start without one. `make
run-fake` sets a local value, since the fake met.no needs no real contact details.

### Trying it
```sh
curl -i http://localhost:8080/healthz
```
Then try the requests in [Example usage](#example-usage), with `start` and `end` changed to times within the next 7 days.

Stop the service with Ctrl-C. It stops taking requests, lets running ones finish, then cancels met.no calls and exits.
Without `make`, also stop the fake met.no (`kill %1`).

## Example usage
The examples assume the service runs locally on port 8080 (see [How to run](#how-to-run)) and that the time is
`2026-11-02T09:00:00Z`.

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
                                ┌─ how-is-the-weather-at-api ─┐
Phone app ──HTTPS──▶ load ──────┼─ how-is-the-weather-at-api ─┼──HTTPS──▶ api.met.no
                     balancer   └─ how-is-the-weather-at-api ─┘
                                              │
                                            Redis   forecasts, upstream budget, pauses, leases, backoff
```

The instances are identical. Each validates requests, applies the freshness rules and calls met.no, and all of them keep their
state in Redis.

- **Our service is the proxy the met.no ToS asks for.** Phones never call met.no directly.
- **It scales out.** At ~80 req/s one instance handles the load, but several give availability: deploys without downtime, and
  surviving the loss of an instance. Because the state is shared (see [Shared state](#shared-state)), the instances behave like
  one service towards met.no.
- **The phone app is ours, but the server does not trust it.** All input comes from the client and is validated. Every response
  depends only on the request URL, the current time and met.no's data, so one client can't change what another client sees.
- **Forecasts are cached by location (coordinates truncated to 4 decimals), not by eventId.** All users of an event send the
  same coordinates, so the cache holds about one entry per event, and events at the same venue share one entry.
- **Freshness comes from met.no's `Expires` header, capped at 2 hours.** We never call met.no for a location before `Expires`.
  After that, we send a conditional request with `If-Modified-Since` set to the stored `Last-Modified`. Users never see data more
  than 2 hours after met.no last confirmed it (see [Freshness](#freshness)).
- **Upstream traffic is capped at half of met.no's 20 req/s per application** by one budget shared by all instances. Known
  locations go first, so abuse can only delay brand-new events (see [Upstream budget](#upstream-budget)). Concurrent requests
  for the same location share one upstream call, and errors from met.no pause or slow down our calls
  (see [Talking to met.no](#talking-to-metno)).

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

The cache is the [shared state](#shared-state), so a forecast fetched by one instance serves all of them. Cache state of a
location at time `now` (only looked at for `upcoming` and `live` events):

| State | When | What we do |
|---|---|---|
| Fresh | `now < Expires` | Serve from cache. No met.no request. |
| Refreshing | `Expires ≤ now < deadline` | Serve from cache right away, and start one background refresh. |
| Past deadline | `now ≥ deadline` | Treated as not cached. The request waits for a refresh (with a timeout). |
| Missing | not in cache | The request waits for a fetch (with a timeout). |

Data younger than 2 hours is always served, also when met.no fails, calls are paused or a location is backed off. Older data is
never served.

- **Failed background refresh** (5xx, timeout, network error): we keep serving until `deadline` and retry with per-location
  backoff (see [Talking to met.no](#talking-to-metno)). The backoff is capped so at least one retry happens before `deadline`.
- **Failed refresh while the request waits**: `502`, or `503` if the upstream budget is used up or met.no asked us to back off.
  A request waits at most 3 seconds and then gets `503` with `Retry-After: 1`; the met.no call keeps running so the next request
  can use the result.
- **`Expires` later than `deadline`**: the ToS wins. We don't call met.no before `Expires`, and we don't serve the data after
  `deadline`, so in that gap the location is unavailable (`503`). This should almost never happen; it is logged once per
  location and fetch.
- **A met.no `429` pause longer than an entry's remaining time**: the entry passes its deadline and returns `503`.
- **`Expires` already past when the answer arrives** (e.g. our clock is ahead of met.no's): a location is refreshed at most once
  a minute, so unchanged data is not fetched again on every request.

The 2-hour limit also covers copies kept by the phone or any cache in front of us, so the response headers never let a copy live
past `deadline` (see [Response headers](#response-headers)). The app is expected to request the weather when the event screen
opens, and again if the screen stays open past `max-age`. On `503` it should retry after `Retry-After`, plus a small random delay
so phones don't all retry in the same second.

## Talking to met.no
Every request to met.no:
- goes to `https://api.met.no/weatherapi/locationforecast/2.0/compact`. Plain `http` is only allowed through `-upstream-url`, so
  a local fake met.no can be used for development and tests.
- sends the `-user-agent` value, and `If-Modified-Since` when we have a `Last-Modified`.
- accepts gzip (Go's client handles it, as long as we don't set `Accept-Encoding` ourselves).
- has coordinates with at most 4 decimals, because they are built from our integer coordinates
  (see [Input parameters](#input-parameters)).
- has a 10s timeout.

How we react to met.no's answers:

| Answer | Scope | What we do |
|---|---|---|
| `200`, `304` | location | Store the data, `Last-Modified` and `Expires`, and set `validated_at`. Reset the location's backoff and the global failure count. A `304` uses the copy the call started from, so it still works if the stored copy expired meanwhile. |
| `203` | — | Use the data as normal. The product version is deprecated, so we log a warning (at most once per hour). Alert on that log: we need to migrate. |
| `429` | all calls | Pause all calls for `Retry-After` (60s if missing). Log a warning. |
| `403` | all calls | Pause all calls for 15 min, then send one probe request from one instance (as soon as the budget has a token for it). Another `403` doubles the pause, up to 1h. Log an error with the status and the start of the response body. A human must look at it: it means we are blocked or our identification is wrong. |
| Other `4xx` | location | Our request is wrong. Log an error and back off the location for the maximum time (5 min). |
| `5xx`, timeout, network error | location + global | Back off the location (30s, doubling to 5 min, with random jitter). After 5 failures in a row across all locations, pause all calls for 30s, doubling to 5 min. The first success resets it. |

- **A location in backoff gets no met.no calls.** Until the backoff ends, requests without servable data get `502` with
  `Retry-After` at once. Without this, a failing location would be fetched again on every request, since shared upstream calls
  only cover requests that overlap in time.
- **During a pause** (`429`, `403` or the global failure pause), requests without servable data get `503` with `Retry-After` set
  to the remaining pause. `/readyz` stays healthy (see [Health checks](#health-checks)).

### Upstream budget
All met.no calls from all instances share one budget: a token bucket that refills at `-upstream-rate` (default 10/s) and holds at
most 5 tokens. Adding instances doesn't add budget. Each call takes one token. There are two kinds of calls:

| Kind | Examples | Rule |
|---|---|---|
| Known location | background refresh, entry past its deadline | May take any token. |
| New location | first request for a location | May only take a token while at least 3 are left, so it never takes the last 2. |

- **Known locations always go first.** Spamming new coordinates can never take the last 2 tokens, so existing events keep being
  refreshed. When Redis starts empty nothing is known, so new locations get nearly the whole budget. Restarting or deploying
  instances doesn't empty the cache, since it lives in Redis.
- **No waiting.** A background refresh without a token is skipped; the next request for that location tries again. A request that
  needs a met.no call and gets no token fails at once with `503` and `Retry-After: 1`. Requests for the same location share one
  call, so many users opening the same new event use one token.
- **Why 10/s and not 20/s:** the limit is per application, across all our environments. The shared bucket covers every instance
  of one environment, during deploys too, but staging and local development have their own Redis or run in memory, so their
  calls add to production's. The remaining 10/s leaves room for them, and they should run with a much lower `-upstream-rate`,
  e.g. 1, or against the fake met.no.
- **Why 5 tokens:** a full bucket allows a burst. With 5 tokens, the worst case in any second is 5 + 10 = 15 calls, safely under
  20. The same burst is why `-upstream-rate` is capped at 15: at that rate the worst case is 5 + 15 = 20.
- **Jitter:** a location may refresh at `Expires` plus a random 0–60s. met.no often gives many locations the same `Expires`, and
  the ToS asks us not to send synchronised requests. This only delays calls, so we still never call before `Expires`.
- **Capacity limit:** if known locations alone need the whole budget, new events get `503`. With a typical `Expires` of 30–60 min,
  10/s keeps about 18 000–36 000 events viewed at the same time fresh. Beyond that we would need a special agreement with met.no.
  Budget usage should be alerted on well before that point, once it is measured (see [Further work](#further-work--considerations)).

## Shared state
All state that the met.no rules depend on lives in Redis, so the rules hold for the whole service, not per instance:

| State | In Redis |
|---|---|
| Forecasts | One key per location: the time steps, `validated_at`, `Expires`, the refresh time and `Last-Modified`. It expires a minute after neither the deadline nor `Expires` plus jitter needs it, so there is no cleanup job. About 4 KB per location. |
| [Upstream budget](#upstream-budget) | One token bucket, refilled and taken from in one atomic step by a Lua script. |
| Pauses | A key with the end time, checked by every instance before calling met.no. |
| The probe after a `403` | A lease, so exactly one instance sends it. |
| Failures in a row | A counter. With concurrent calls the count is approximate, which can only make us pause sooner. |
| One met.no call per location | A 15 s lease per location. It outlives the 10 s met.no timeout, so if its instance crashes, it expires and another instance takes over. Within an instance, a map of calls in progress makes sure only one goroutine asks Redis. |
| Per-location backoff | One key per location with the end of the backoff and the next backoff length. It expires 6 minutes after the last failure, so the doubling resets after a quiet spell. |
| "Log once" warnings | A key that expires after the period, so a warning is logged once for the whole service. |

- **Waiting for another instance's fetch.** A request for a location that has no usable data and whose lease is held elsewhere
  polls Redis every 100 ms, for at most the 3 seconds a request waits anyway.
- **Clocks.** Times are passed to Redis by the instances rather than read from Redis' clock. With NTP, instance clocks differ by
  milliseconds, which doesn't matter for a 2-hour rule or for the budget, and the budget never moves back in time, so a clock
  that is behind can't refill it twice. This also lets the same rules be tested on fake time against both versions of the state.
- **When Redis is down, we fail closed.** Without Redis no cached data can be read and no met.no call can be coordinated, and
  falling back to per-instance state would make the 20 req/s limit depend on how many instances run. Calls to Redis time out
  after 200 ms. `upcoming` and `live` requests then get `503` with `Retry-After: 5`, while `forecast_pending`, `ended` and `400`
  responses still work, since they don't need Redis. `/readyz` fails (see [Health checks](#health-checks)), and the outage is
  logged once.
- **In-memory mode** (`-in-memory`) keeps the same state in the process instead, for tests and local runs. It must only run as a
  single instance: several in-memory instances would each have their own budget. That's why it has to be asked for explicitly,
  and a missing `-redis-url` doesn't silently fall back to it.

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
- Coordinates are stored as integers in units of 1/10 000 degree. `10.75`, `10.7500` and `10.750012` are the same cache key, and
  the met.no URL is formatted from the integers.
- The response's `location` shows the truncated coordinates that were used.

## API response
The response is always JSON with the same structure, giving air temperature in Celsius and wind speed in m/s
(see [Example usage](#example-usage) for full responses). Its content depends on the event's phase at the time of the request:

| Phase | When | `current` | `forecast` |
|---|---|---|---|
| `forecast_pending` | `start` is more than 7 days away | `null` | `[]`, with `available_from` set to `start − 7 days` |
| `upcoming` | `start` is within the next 7 days | weather at the venue right now | entries around the event (see below) |
| `live` | between `start` and `end` | weather right now | entries from now until `end` |
| `ended` | after `end` | `null` | `[]` |

The app only needs to look at `phase`. The 7-day window comes from the requirements ("events that start in the next 7 days"), not
from met.no, which forecasts about 9 days ahead; it is a constant, so it can't be raised past what met.no covers. `live` and
`ended` go beyond the requirements, because users also open the event screen during and after an event.

- All times are UTC.
- `fetched_at` is `validated_at`: when met.no last confirmed the data. It is `null` when no met.no data was needed.
- met.no forecasts are hourly for about the first 2.5 days and 6-hourly (00, 06, 12, 18 UTC) after that. Each entry is the
  forecast for that exact time, so we include the forecast steps inside the event plus the nearest step before and after it.
  An hourly 18:00–20:00 event gets 18, 19 and 20. A 6-hourly 19:00–21:00 event gets 18 and 00, so it still gets a forecast.
  A `live` event uses the same rule with now in place of `start`. If the event runs past met.no's data, the list ends where the
  data ends.

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
  the time until `available_from` and S is 0. An `ended` response never changes, so N is 1 day and S is 0. There is no
  `stale-if-error`, since that would let a copy outlive `deadline`.
- No `Expires`, `Last-Modified` or `ETag` header, and no `304` responses. Our response depends on the current time as well as
  met.no's data (`current`, `phase`, and a `live` forecast that shrinks over time), so met.no's `Last-Modified` can't validate it.
  Responses are about 1 KB, so conditional requests would save little. `max-age` is relative, so it doesn't depend on the phone's
  clock.
- `Link: <https://creativecommons.org/licenses/by/4.0/>; rel="license"`. Together with the `attribution` field, this meets the
  CC BY 4.0 credit requirement.

### Status codes
| Code | Meaning |
|---|---|
| 200 | Weather returned. `forecast_pending` and `ended` events return 200 with `current: null` and `forecast: []`. |
| 400 | Invalid input (see [Input parameters](#input-parameters)). |
| 502 | met.no failed for this location, now or recently. Includes `Retry-After` when the location is backed off. |
| 503 | met.no can't be called right now: no budget token, calls paused, `Expires` not passed yet, the call took over 3 seconds, or Redis can't be reached. Includes `Retry-After`. |

502 and 503 only happen when there is no data younger than 2 hours (see [Freshness](#freshness)). Error responses have
`Cache-Control: no-store` and a JSON body with a single `error` message (see [Invalid input](#invalid-input)).

### Health checks
- `GET /healthz` answers `200` while the process runs.
- `GET /readyz` answers `503` once shutdown has started, and when the instance can't reach Redis, so the load balancer sends
  traffic to instances that can. It stays `200` while met.no calls are paused: cached data can still be served, and taking every
  instance out of traffic would serve nothing. A pause is met.no's problem; losing Redis is the instance's.

## OpenAPI specification
The API is described in [`openapi.yaml`](openapi.yaml), which can be used to generate clients. It uses OpenAPI 3.0.3 rather than
3.1, because client generators support 3.0 better. For example, a Kotlin client:

```sh
npx @openapitools/openapi-generator-cli generate -i openapi.yaml -g kotlin -o clients/kotlin
```

- `lat` and `lon` are strings in the spec, not numbers. The server only accepts plain decimal numbers, and many languages
  format small floating-point numbers with an exponent (e.g. `5e-05`), which would be rejected.
- Check the spec with `npx @redocly/cli lint openapi.yaml`.

## Production and operations
How the service is meant to run in production. Nothing here is set up yet; it is the plan to discuss.

**Running the instances**
- The container image runs on any container platform: at least 3 instances spread over availability zones, so a deploy or the
  loss of a zone never takes the weather down. Adding instances adds capacity for phones, not traffic towards met.no.
- `/readyz` is the readiness check and `/healthz` the liveness check (see [Health checks](#health-checks)).
- Rolling deploys replace one instance at a time. On `SIGTERM` an instance fails `/readyz`, finishes running requests, cancels
  met.no calls and exits within 20 seconds, so the platform's grace period must be longer than that. The cache lives in Redis,
  so deploys don't empty it.
- Each instance needs little: about 0.1 CPU and 64 MB at the expected ~80 req/s across all instances.

**Redis**
- A managed Redis (or Valkey) with a replica and automatic failover, in the same region as the instances. A Redis outage makes
  `upcoming` and `live` requests fail closed, so its availability matters more than its size.
- About 4 KB per location, so 36 000 locations take roughly 150 MB.
- `maxmemory-policy noeviction`. Evicting keys could drop a pause or the budget, which would break the met.no rules; with
  `noeviction` a full Redis makes writes fail, and the service fails closed instead. Alert on memory use well before that.
- Password and TLS through the URL (`rediss://user:password@host:6380`), with the URL from the platform's secret store.

**Environments**
- Each environment has its own Redis. Staging runs against the fake met.no, or with `-upstream-rate 1`, because every
  environment counts towards the same 20 req/s per application (see [Upstream budget](#upstream-budget)).
- The production User-Agent names the service and a team address, not a person, e.g.
  `how-is-the-weather-at-api/1.0 weather-team@example.com`.

**Security**
- TLS ends at the load balancer. Only the instances can reach Redis, and the only outbound traffic they need is to
  `api.met.no`.

**Build and deploy**
- Every pull request runs `make lint` and `make test`. Merges to `main` build the image, tagged with the commit, deploy it to
  staging and then to production. Rolling back means deploying the previous image.

**Observability**
- Logs are JSON on stderr, ready for the platform's log collection.
- Metrics and alerts are the next step (see [Further work](#further-work--considerations)). The alerts that matter most: met.no
  `403` (blocked) and `203` (deprecated version), Redis unreachable, the rate of `503` responses, and upstream budget usage.

## Further work & considerations

Deliberately left out for now:
- **Falling back to per-instance state when Redis is down.** Rejected; see [Shared state](#shared-state).
- **Trusted event data.** Events come from the client unverified. The long-term fix is to look events up in an authoritative
  source, or to have the system that hands events to the app sign them so we can verify the signature.
- **Per-client rate limiting.** Phones often share IP addresses behind carrier NAT, so a per-IP limit must be generous.
- **A caching layer/CDN in front.** Our headers already support one. URLs that differ only in extra coordinate decimals are
  cached separately there; our own cache key is not affected.
- **Raw lat/lon endpoint.** Removed in favour of events-only, to keep the attack surface small.
- **Background prefetching before predictable bursts.** It amounts to polling met.no. If it's ever added, requests must be spread
  out with random delays as the ToS requires.
- **Configuration via env vars or config files.** Flags only, by choice, except `PORT` (see [Configuration](#configuration)).
- **A per-event location limit** (e.g. at most 5 locations per eventId, otherwise `409`). It can't stop abuse while eventIds are
  unverified, since an attacker just uses a new eventId, and it turns app bugs into user-facing errors. A bug such as sending the
  phone's GPS position instead of the event's location shows up as a rising new-location rate and a falling cache hit rate.

Open points and possible improvements:
- Finalise the eventId format and the maximum event length (14 days is a placeholder).
- Send `altitude` to met.no for more accurate temperatures. This needs a trusted source for venue altitude.
- More weather data (precipitation, weather symbol, uncertainty ranges from the `/complete` endpoint).
- Redis availability: a managed Redis (or Valkey) with a replica and automatic failover, since a Redis outage takes the weather
  down. Persistence is optional: an empty Redis only means warming up again (about 2 000 locations per 3½ minutes at 10/s).
- Metrics and alerts: the service logs the events in [Talking to met.no](#talking-to-metno) and Redis outages, but has no metrics
  yet. Next would be cache hit rate, new-location rate, upstream budget usage, 502/503 counts, met.no status codes, pause and
  backoff counts, Redis errors and latency, and requests that gave up waiting, with alerts on `403`, `203`, budget usage and
  Redis outages.
- Use 3 decimals (~110 m) for the cache key instead of 4 (~11 m). met.no's model grid is about 1 km, so nearby venues could share
  an entry and a met.no call without losing accuracy. Do it once we can measure the cache hit rate.
