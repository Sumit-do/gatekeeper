# gatekeeper

[![CI](https://github.com/sumit-do/gatekeeper/actions/workflows/ci.yml/badge.svg)](https://github.com/sumit-do/gatekeeper/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/sumit-do/gatekeeper?sort=semver)](https://github.com/sumit-do/gatekeeper/releases/latest)
[![Go Reference](https://pkg.go.dev/badge/github.com/sumit-do/gatekeeper.svg)](https://pkg.go.dev/github.com/sumit-do/gatekeeper)
![Go](https://img.shields.io/badge/go-1.24+-00ADD8?logo=go&logoColor=white)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](LICENSE)

A lightweight HTTP API gateway and reverse proxy written in Go with **zero
third-party dependencies**. It sits in front of your services and gives you
prefix-based routing, round-robin load balancing, per-upstream circuit
breakers, per-client token-bucket rate limiting, request IDs, structured JSON
logs, Prometheus metrics, hot config reload and graceful shutdown — in one
static binary.

## Features

- **Routing** by longest matching path prefix, with optional prefix stripping.
- **Load balancing** round-robin across any number of upstreams per route.
- **Circuit breakers** per upstream (closed / open / half-open) with
  configurable failure threshold and cooldown; unhealthy upstreams are skipped
  automatically and probed once the cooldown passes.
- **Rate limiting** with a token bucket per client key (`X-API-Key` header,
  falling back to client IP) per route. Rejections are `429` with a
  `Retry-After` header. Idle buckets are evicted.
- **Middleware chain**: `X-Request-ID` generation and propagation, structured
  logging via `log/slog` (JSON), panic recovery, per-route timeouts, response
  metrics.
- **Admin listener**: `/healthz`, `/readyz`, `/metrics` (Prometheus text
  format) and `POST /admin/reload` to swap in a new config without downtime.
- **Graceful shutdown** on `SIGINT` / `SIGTERM` with a drain timeout.
- **Standard library only.** `go.mod` has no `require` block.

## Architecture

```mermaid
flowchart LR
    C[Client] -->|HTTP| RID

    subgraph GW["gatekeeper (listen :8080)"]
        direction LR
        RID[Request ID] --> LOG[slog JSON logging] --> REC[Panic recovery] --> RT{Router<br/>longest prefix}
        RT --> M[Metrics] --> TO[Timeout] --> RL[Rate limiter<br/>token bucket per key] --> LB[Round-robin picker]
        LB --> CB1[Breaker A]
        LB --> CB2[Breaker B]
    end

    CB1 -->|httputil.ReverseProxy| U1[(Upstream A)]
    CB2 -->|httputil.ReverseProxy| U2[(Upstream B)]

    subgraph ADM["admin (:9090)"]
        H["/healthz  /readyz"]
        P["/metrics"]
        R["POST /admin/reload"]
    end
    R -.swap atomic.Pointer.-> RT
    P -.reads.-> M
```

Request flow:

1. **Global middleware** (`internal/middleware`) assigns or preserves
   `X-Request-ID`, logs one JSON line per request and converts panics into
   `500`s.
2. The **router** (`internal/proxy.Router`) picks the route with the longest
   prefix that matches on a `/` segment boundary, or returns `404`.
3. **Per-route middleware** records metrics and attaches a context deadline.
4. The **rate limiter** (`internal/ratelimit`) looks up the bucket for the
   client key and either admits the request or answers `429 Retry-After`.
5. The **picker** walks upstreams round-robin and asks each **breaker**
   (`internal/breaker`) whether it may send; if every circuit is open the
   gateway returns `503`.
6. `httputil.ReverseProxy` forwards the request. A `5xx` or transport error
   counts as a breaker failure; a deadline becomes `504`, any other transport
   error `502`.

Package layout:

```
cmd/gatekeeper/      main, admin endpoints, hot-reloadable gateway wrapper
internal/config/     JSON schema, defaults, validation
internal/proxy/      Router, Route, Upstream, reverse proxy glue
internal/ratelimit/  keyed token-bucket limiter with eviction
internal/breaker/    circuit breaker state machine (injectable clock)
internal/middleware/ request ID, logging, recovery, timeout, metrics
internal/metrics/    hand-rolled Prometheus text exposition
examples/echo/       tiny upstream used by docker-compose and demos
```

## Install

Download a prebuilt binary for Linux, macOS or Windows from the
[latest release](https://github.com/sumit-do/gatekeeper/releases/latest), or build from source:

```sh
go install github.com/sumit-do/gatekeeper/cmd/gatekeeper@latest
gatekeeper -version
```

## Quick start

Requires Go 1.24+.

```sh
git clone https://github.com/sumit-do/gatekeeper
cd gatekeeper

# terminal 1: two echo upstreams on :8081 and :8082
make run-echo

# terminal 2: the gateway, using examples/gatekeeper.json
make run
```

Or with Docker (gateway plus two echo instances):

```sh
docker compose up --build
```

Then:

```sh
curl -i localhost:8080/echo/hello
```

## Configuration

Gatekeeper reads a single JSON file (default `gatekeeper.json`, override with
`-config path`). Unknown fields are rejected so typos fail fast. Durations are
strings accepted by `time.ParseDuration` (`"500ms"`, `"30s"`, `"5m"`).

```json
{
  "listen": ":8080",
  "admin": ":9090",
  "log_level": "info",
  "shutdown_timeout": "15s",
  "routes": [
    {
      "name": "echo",
      "path_prefix": "/echo",
      "upstreams": ["http://127.0.0.1:8081", "http://127.0.0.1:8082"],
      "strip_prefix": true,
      "rate_limit": { "rps": 5, "burst": 10, "idle_timeout": "5m" },
      "timeout": "5s",
      "circuit_breaker": { "failure_threshold": 3, "cooldown": "20s" }
    }
  ]
}
```

### Top level

| Field              | Type     | Default  | Description                                                     |
| ------------------ | -------- | -------- | --------------------------------------------------------------- |
| `listen`           | string   | `:8080`  | Address for proxied traffic.                                    |
| `admin`            | string   | `:9090`  | Address for `/healthz`, `/readyz`, `/metrics`, `/admin/reload`. |
| `log_level`        | string   | `info`   | `debug`, `info`, `warn` or `error`.                             |
| `shutdown_timeout` | duration | `10s`    | How long in-flight requests may drain on SIGINT/SIGTERM.       |
| `routes`           | array    | required | At least one route.                                             |
| `api_keys`         | array    | none     | Accepted `X-API-Key` credentials, each `{"name", "key"}`; keys need 16+ characters. When empty, the header is ignored and clients are keyed by IP. |

### Route

| Field                              | Type     | Default       | Description                                                                 |
| ---------------------------------- | -------- | ------------- | --------------------------------------------------------------------------- |
| `name`                             | string   | `path_prefix` | Label used in logs and metrics.                                             |
| `path_prefix`                      | string   | required      | Must start with `/`. Matches on segment boundaries (`/api` ≠ `/apix`).     |
| `upstreams`                        | []string | required      | Absolute `http`/`https` URLs, load-balanced round-robin.                   |
| `strip_prefix`                     | bool     | `false`       | Remove `path_prefix` before forwarding (`/api/users` → `/users`).          |
| `require_api_key`                  | bool     | `false`       | Reject requests without a valid `X-API-Key` with `401`. Needs `api_keys`.   |
| `timeout`                          | duration | `30s`         | End-to-end deadline per request; exceeded → `504`.                          |
| `rate_limit`                       | object   | none          | Omit to disable rate limiting on this route.                                |
| `rate_limit.rps`                   | number   | required      | Sustained requests per second per client key.                               |
| `rate_limit.burst`                 | int      | required      | Bucket capacity (maximum instantaneous burst).                              |
| `rate_limit.idle_timeout`          | duration | `5m`          | Buckets unused for this long are evicted.                                   |
| `circuit_breaker.failure_threshold`| int      | `5`           | Consecutive failures (5xx or transport error) before the circuit opens.     |
| `circuit_breaker.cooldown`         | duration | `30s`         | Time the circuit stays open before admitting one probe request.             |

Validation errors are aggregated and point at the offending route:

```
config examples/gatekeeper.json: invalid config:
routes[0] (echo): rate_limit.burst must be >= 1, got 0
routes[1] (/httpbin): upstream "ftp://x": scheme must be http or https, got "ftp"
```

## Example session

```sh
$ curl -s -i localhost:8080/echo/hello | head -5
HTTP/1.1 200 OK
Content-Type: application/json
X-Request-Id: 3f1c0d2a9b8e4e6f8a7b6c5d4e3f2a1b
X-Upstream: echo1

$ curl -s localhost:8080/echo/hello -H 'X-Request-ID: trace-42' | jq .headers
{ "X-Forwarded-For": ["172.18.0.1"], "X-Request-Id": ["trace-42"], ... }

# Round-robin: alternate upstreams on consecutive calls
$ for i in 1 2 3 4; do curl -s localhost:8080/echo/ | jq -r .name; done
echo1
echo2
echo1
echo2

# Burst past the limit and get told when to come back
$ for i in $(seq 1 12); do curl -s -o /dev/null -w '%{http_code} ' localhost:8080/echo/ -H 'X-API-Key: demo-key-0123456789'; done
200 200 200 200 200 200 200 200 200 200 429 429
$ curl -s -i localhost:8080/echo/ -H 'X-API-Key: demo-key-0123456789' | grep -i retry
Retry-After: 1

# An invented key is rejected instead of getting its own fresh bucket
$ curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/echo/ -H 'X-API-Key: made-up-key-123456'
401

# Trip a breaker: ask echo to return 500 three times, then watch the gauge
$ for i in 1 2 3 4 5 6; do curl -s -o /dev/null -w '%{http_code}\n' 'localhost:8080/echo/?status=500'; done
$ curl -s localhost:9090/metrics | grep circuit_state
gatekeeper_circuit_state{route="echo",upstream="http://echo1:8081"} 1
gatekeeper_circuit_state{route="echo",upstream="http://echo2:8081"} 1

$ curl -s -i localhost:9090/readyz | head -1
HTTP/1.1 503 Service Unavailable

# Edit the config, then reload without dropping connections
$ curl -s -X POST localhost:9090/admin/reload
{"routes":1,"status":"reloaded"}
```

Metrics exposed on `/metrics`:

| Metric                                      | Type      | Labels              |
| ------------------------------------------- | --------- | ------------------- |
| `gatekeeper_requests_total`                 | counter   | `route`, `status`   |
| `gatekeeper_request_duration_seconds`       | histogram | `route`, `le`       |
| `gatekeeper_rate_limited_total`             | counter   | `route`             |
| `gatekeeper_circuit_state` (0/1/2 = closed/open/half-open) | gauge | `route`, `upstream` |

## Design decisions

- **Standard library only.** `net/http/httputil.ReverseProxy` already does
  the hard parts (hop-by-hop headers, streaming, `X-Forwarded-*`), `log/slog`
  gives structured logging, and the Prometheus text format is simple enough
  to render by hand. The result is a dependency-free, auditable binary.
- **Small packages with narrow interfaces.** `breaker`, `ratelimit` and
  `metrics` know nothing about HTTP, so they are trivially unit-testable;
  `proxy` composes them. Both the breaker and the limiter take an injectable
  clock, so state transitions and token maths are tested deterministically
  without `time.Sleep`.
- **Breaker outcome from the proxy's own hooks.** `ModifyResponse` marks any
  `5xx` as a failure and `ErrorHandler` marks transport errors, so no extra
  round-trip wrapper is needed and every admitted request reports exactly one
  outcome.
- **Half-open admits a single probe.** Rather than letting a burst of traffic
  hit a recovering upstream, exactly one in-flight request decides whether
  the circuit closes again.
- **API keys are verified, never trusted blindly.** A naive limiter that
  buckets by whatever `X-API-Key` the client sends is trivially bypassed:
  send a new random key with every request and you get a fresh bucket each
  time. Gatekeeper only honours keys listed in `api_keys`; unknown keys get
  `401`, and with no keys configured the header is ignored and the client IP
  is used. Keys are looked up by SHA-256 digest so lookup time does not leak
  how much of a guessed key is right, and the secret is stripped before
  forwarding, replaced by a verified `X-Gatekeeper-Client: <name>` header
  (any client-supplied copy of that header is removed first).
- **Client IP from the connection only.** `X-Forwarded-For` is not honoured
  because it is trivially spoofable. Put gatekeeper behind a trusted TLS
  terminator if you need forwarded IPs, and extend `proxy.clientIP`.
- **Hot reload via `atomic.Pointer[Router]`.** A new router is built from the
  file and swapped in atomically; a bad config is rejected with `400` and the
  old router keeps serving. In-flight requests finish on the router they
  started on.
- **Timeout as a context deadline, not `http.TimeoutHandler`.** The latter
  buffers the whole response; a deadline lets the proxy stream and still map
  the failure to `504`.
- **Longest-prefix matching on segment boundaries** avoids `/api` capturing
  `/apix` and makes route order in the file irrelevant.

## Development

```sh
make lint   # go vet + gofmt check
make test   # go test -race ./...
make build  # static binary in bin/
make docker # build the container image
```

## Roadmap

- TLS termination and optional mTLS to upstreams.
- Active health checks (periodic probes independent of live traffic).
- Additional balancing strategies: least-connections, weighted, consistent hash.
- Retries with budget for idempotent methods.
- Request/response header rewriting rules per route.
- JWT validation as a second auth mode alongside API keys.
- Config file watching (fsnotify-free polling) as an alternative to `POST /admin/reload`.
- Per-route access logs and sampling.

## License

MIT — see [LICENSE](LICENSE).
