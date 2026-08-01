# GitHub REST API Proxy

A high-performance reverse proxy for the GitHub REST API that provides authentication balancing, rate limiting, caching, and monitoring capabilities.

## Features

- **Authentication Balancing**: Distribute requests across multiple GitHub tokens/apps
- **Rate Limiting**: Built-in rate limiting with configurable requests per second
- **Caching**: Multiple storage backends (in-memory, PebbleDB, BoltDB, S3, Redis) for response caching using [bored-engineer/github-conditional-http-transport](https://github.com/bored-engineer/github-conditional-http-transport)
- **Monitoring**: Prometheus metrics for rate limit tracking

## Installation

```bash
go install github.com/bored-engineer/github-api-proxy@latest
```

Or build from source:

```bash
git clone https://github.com/bored-engineer/github-api-proxy.git
cd github-api-proxy
go build -o github-api-proxy .
```

## Usage

### Basic Usage

```bash
# Start with default settings (listens on 127.0.0.1:44879)
./github-api-proxy

# Start with custom listen address
./github-api-proxy --listen 0.0.0.0:8080

# Start with TLS
mkcert github-api-proxy.localhost
echo "127.0.0.1 github-api-proxy.localhost" | sudo tee -a /etc/hosts
./github-api-proxy --tls-cert ./github-api-proxy.localhost.pem --tls-key ./github-api-proxy.localhost-key.pem
```

### Authentication

The proxy supports multiple authentication methods that can be used simultaneously:

#### Personal Access Tokens
```bash
./github-api-proxy --auth-token "ghp_your_token_here"
```

#### OAuth Apps
```bash
./github-api-proxy --auth-oauth "client_id:client_secret"
```

#### GitHub Apps
```bash
./github-api-proxy --auth-app "client_id:installation_id:private_key"
```

#### Multiple Authentication Methods
```bash
./github-api-proxy \
  --auth-token "ghp_token1" \
  --auth-token "ghp_token2" \
  --auth-oauth "client1:secret1" \
  --auth-app "app1:install1:key1"
```

### Caching

Caching is disabled by default; pass one of the flags below to enable it.

#### In-Memory
```bash
./github-api-proxy --cache-memory
```

#### Pebble
```bash
./github-api-proxy --cache-pebble-db /path/to/cache.db
```

#### BoltDB
```bash
./github-api-proxy --cache-bbolt-db /path/to/cache.db --cache-bbolt-bucket my-bucket
```

#### Redis
```bash
./github-api-proxy --cache-redis-addr 127.0.0.1:6379
```

#### S3
```bash
./github-api-proxy \
  --cache-s3-bucket github-rest-api-proxy \
  --cache-s3-region us-west-2 \
  --cache-s3-prefix cache/
```

### Rate Limiting

```bash
# Limit to 5000 requests per hour, shared globally across all authentication tokens
./github-api-proxy --rph 5000
```

### Custom GitHub API URL

```bash
# Use GitHub Enterprise
./github-api-proxy --url "https://github.company.com/api/v3/"
```

### Source IP Balancing

```bash
# Round-robin outgoing requests across multiple source IP addresses
./github-api-proxy --src-ip 203.0.113.10 --src-ip 203.0.113.11
```

## Configuration Options

| Flag | Description | Default |
|------|-------------|---------|
| `--listen` | Address to listen on | `127.0.0.1:44879` |
| `--url` | GitHub API URL | `https://api.github.com/` |
| `--tls-cert` | TLS certificate file | (disabled) |
| `--tls-key` | TLS key file | (disabled) |
| `--auth-token` | GitHub personal access token | (none) |
| `--auth-oauth` | OAuth client ID/secret (format: `client_id:client_secret`) | (none) |
| `--auth-app` | GitHub App clients (format: `client_id:installation_id:private_key`) | (none) |
| `--rph` | Maximum requests per hour, shared globally across all authentication tokens | (unlimited) |
| `--rate-interval` | Interval for rate limit checks | `1m0s` |
| `--rate-resources` | Resource types to report rate limit metrics for (empty means report all) | `core,graphql` |
| `--rate-reserve` | Proactively reserve rate limit capacity for in-flight requests before response headers are parsed | `true` |
| `--rate-spoof` | Return a synthetic 429 response instead of forwarding requests once a credential's rate limit is exhausted | `true` |
| `--src-ip` | Source IP addresses to balance outgoing requests across (round-robin) | (none) |
| `--cache-memory` | Use an in-memory cache for conditional requests | `false` |
| `--cache-bbolt-db` | Path to BoltDB for caching | (disabled) |
| `--cache-bbolt-bucket` | BoltDB bucket name | `github-api-proxy` |
| `--cache-pebble-db` | Path to PebbleDB for caching | (disabled) |
| `--cache-s3-bucket` | S3 bucket for caching | (disabled) |
| `--cache-s3-region` | S3 region | (AWS default) |
| `--cache-s3-endpoint` | S3 endpoint (for MinIO, etc.) | (AWS default) |
| `--cache-s3-prefix` | S3 key prefix | (none) |
| `--cache-redis-addr` | Redis address for caching | (disabled) |
| `--cache-redis-username` | Redis username | (none) |
| `--cache-redis-password` | Redis password | (none) |
| `--cache-redis-db` | Redis database number | `0` |
| `--retries` | Maximum retries for requests without a body that fail with a network error or a 429/5xx status code (`0` disables retries) | `2` |
| `--retry-wait` | Initial backoff delay between retries (doubles each attempt, subject to `--retry-wait-max`) | `250ms` |
| `--retry-wait-max` | Maximum backoff delay between retries | `30s` |
| `--rate-retry` | Retry 429 responses until `Retry-After` clears, instead of counting them against `--retries` | `true` |
| `--log-level` | Minimum log level to output (`trace`, `debug`, `info`, `warn`, `error`) | `info` |
| `--log-rate-limit` | Log requests to the `/rate_limit` API, normally suppressed since they're issued periodically to poll rate limit status rather than in response to real traffic | `false` |
| `--log-addr` | Log the `incoming`/`source` IP and port for each request | `false` |
| `--log-conn` | Log details about the underlying network connection used for each request (remote address, whether it was reused, and idle time) | `false` |
| `--log-request-headers` | Log the full request headers for each request (the `Authorization` value, if any, is always replaced with its hash) | `false` |
| `--log-response-headers` | Log the full response headers for each request (the `Authorization` value, if any, is always replaced with its hash) | `false` |
| `--pprof` | Expose `net/http/pprof` debug endpoints under `/pprof/` (WARNING: allows dumping goroutines, heap, and CPU profiles; do not enable on a publicly reachable listener) | `false` |

## API Endpoints

- `/` - Proxies all requests to the upstream GitHub REST API
- `/metrics` - Prometheus metrics endpoint
- `/pprof/` - `net/http/pprof` debug endpoints, only registered when `--pprof` is set

## Monitoring

The proxy exposes Prometheus metrics at `/metrics`:

- `github_rate_limit_remaining` - Number of requests remaining in current rate limit window
- `github_rate_limit_reset` - Unix timestamp when rate limit window resets

## Logging

Each proxied request is logged as a single structured JSON line. Request- and response-specific fields are grouped into nested `request` and `response` objects:

```json
{
  "level": "info",
  "duration": 283,
  "request": {
    "id": "d9mn11m9b7rlmqkucu8g",
    "method": "GET",
    "scheme": "https",
    "host": "api.github.com",
    "path": "/some/path",
    "query": "page=2",
    "incoming": {
      "ip": "127.0.0.1",
      "port": "60600"
    },
    "conn": {
      "id": "d9mn10u9b7rlmqkucu7g",
      "remote": {
        "ip": "140.82.121.6",
        "port": "443"
      },
      "reused": true,
      "was_idle": true,
      "idle_time": 2032
    },
    "user_agent": "curl/8.7.1",
    "source": {
      "ip": "203.0.113.10"
    },
    "auth": {
      "client_id": "myclient",
      "installation_id": "12345",
      "hashed_token": "ZSx9xofZjJiJME7S5AjHS2EehqQMqlHEtD8d1ZE8XNA="
    }
  },
  "response": {
    "status": 200,
    "cache": {
      "etag": "\"abc123\"",
      "hit": true,
      "stored": false
    },
    "request_id": "ABCD:1234",
    "ratelimit": {
      "limit": 5000,
      "used": 1,
      "remaining": 4999,
      "reset": 1234567890,
      "resource": "core"
    }
  },
  "message": "HTTP request"
}
```

- `request.id` uniquely identifies the incoming request (a [xid](https://github.com/rs/xid)); every attempt of the same request retried by `--retries`/`--rate-retry` shares the same `id`, so they can be correlated across log lines.
- `request.incoming`/`request.source` are only present when `--log-addr` is set. `incoming` is the address of the client that connected to the proxy; `source` is only populated when `--src-ip` was also used, naming the specific source IP that request was dialed from.
- `request.conn` is only present when `--log-conn` is set, describing the underlying network connection used for this specific attempt (via `httptrace`, so retried attempts can show a different connection). `conn.id` is a unique identifier assigned when the connection is dialed and stays the same across every request that reuses it, so they can be correlated in logs. `conn.remote` is the address of the upstream GitHub server actually connected to (e.g. after DNS re-resolution); `conn.reused` reports whether an existing pooled connection was reused instead of dialing a new one; `conn.idle_time` (only present when `conn.was_idle` is `true`) is how long that reused connection had been sitting idle beforehand.
- `request.query` is only present when the request has a query string, logged verbatim.
- `request.headers`/`response.headers` are only present when `--log-request-headers`/`--log-response-headers` are set, respectively. The `Authorization` value (if any) is always replaced with its hash (the same value as `request.auth.hashed_token`) rather than logged raw.
- `request.auth.client_id`/`request.auth.installation_id` are only present for the credential type they apply to (e.g. GitHub Apps set both; OAuth clients set only `client_id`; personal access tokens set neither).
- `request.auth.hashed_token` matches the `hashed_token` field GitHub itself emits in audit log events, computed from the request's `Authorization` header.
- `response.cache` is parsed from the upstream `Cache-Status` header (per [RFC 9211](https://www.rfc-editor.org/rfc/rfc9211)), set by caching (via [bored-engineer/github-conditional-http-transport](https://github.com/bored-engineer/github-conditional-http-transport)) on every response; it's omitted only if that header is missing entirely. `cache.hit` is `true` when a conditional request got back a `304 Not Modified` and the cached body/status was substituted (`response.status` then reflects the substituted status, e.g. `200`, not the upstream `304`). `cache.stored` is `true` when this response was freshly written to the cache. `cache.reason` is the forwarding reason for a non-hit (e.g. `uri-miss` on a first request, or `method`/`bypass` for a request caching doesn't apply to at all) and is absent on a hit. `cache.etag` mirrors the response's `Etag` header, if any.
- `response.ratelimit` is only present when the response carries rate-limit headers (i.e. GitHub API responses), and mirrors `ghratelimit.ParseRate` plus the `X-Ratelimit-Resource` header.
- Requests to `/rate_limit` (issued periodically to poll rate-limit status) are suppressed by default; pass `--log-rate-limit` to include them.
