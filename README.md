# GitHub API Proxy

A high-performance reverse proxy for the GitHub API that provides authentication balancing, rate limiting, caching, and monitoring capabilities.

## Features

- **Authentication Balancing**: Distribute requests across multiple GitHub tokens/apps
- **Rate Limiting**: Built-in rate limiting with configurable requests per second
- **Caching**: Multiple storage backends (in-memory, BoltDB, S3) for response caching using [bored-engineer/github-conditional-http-transport](https://github.com/bored-engineer/github-conditional-http-transport)
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
github-api-proxy

# Start with custom listen address
github-api-proxy --listen 0.0.0.0:8080

# Start with TLS
mkcert github-api-proxy.localhost
echo "127.0.0.1 github-api-proxy.localhost" | sudo tee -a /etc/hosts
github-api-proxy --tls-cert ./github-api-proxy.localhost.pem --tls-key ./github-api-proxy.localhost-key.pem
```

### Authentication

The proxy supports multiple authentication methods that can be used simultaneously:

#### Personal Access Tokens
```bash
./github-api-proxy --auth-tokens "ghp_your_token_here"
```

#### OAuth Apps
```bash
./github-api-proxy --auth-oauth "client_id:client_secret"
```

#### GitHub Apps
```bash
./github-api-proxy --auth-app "app_id:installation_id:private_key"
```

#### Multiple Authentication Methods
```bash
./github-api-proxy \
  --auth-tokens "ghp_token1" \
  --auth-tokens "ghp_token2" \
  --auth-oauth "client1:secret1" \
  --auth-app "app1:install1:key1"
```

### Caching

#### In-Memory (Default)
```bash
./github-api-proxy
```

#### BoltDB
```bash
./github-api-proxy --bbolt-db /path/to/cache.db --bbolt-bucket my-bucket
```

#### S3
```bash
./github-api-proxy \
  --s3-bucket my-cache-bucket \
  --s3-region us-west-2 \
  --s3-prefix github-rest-api-cache/
```

### Rate Limiting

```bash
# Limit to 5000 requests per second per authentication token
./github-api-proxy --rps 5000
```

### Custom GitHub API URL

```bash
# Use GitHub Enterprise
./github-api-proxy --url "https://github.company.com/api/v3/"
```

## Configuration Options

| Flag | Description | Default |
|------|-------------|---------|
| `--listen` | Address to listen on | `127.0.0.1:44879` |
| `--url` | GitHub API URL | `https://api.github.com/` |
| `--tls-cert` | TLS certificate file | (disabled) |
| `--tls-key` | TLS key file | (disabled) |
| `--auth-tokens` | GitHub personal access tokens | (none) |
| `--auth-oauth` | OAuth clients (format: `client_id:client_secret`) | (none) |
| `--auth-app` | GitHub App clients (format: `app_id:installation_id:private_key`) | (none) |
| `--rps` | Maximum requests per second per auth token | (unlimited) |
| `--rate-interval` | Interval for rate limit checks | `1m0s` |
| `--bbolt-db` | Path to BoltDB for caching | (in-memory) |
| `--bbolt-bucket` | BoltDB bucket name | `github-api-proxy` |
| `--s3-bucket` | S3 bucket for caching | (in-memory) |
| `--s3-region` | S3 region | (AWS default) |
| `--s3-endpoint` | S3 endpoint (for MinIO, etc.) | (AWS default) |
| `--s3-prefix` | S3 key prefix | (none) |

## API Endpoints

- `/` - Proxies all requests to GitHub API
- `/metrics` - Prometheus metrics endpoint

## Monitoring

The proxy exposes Prometheus metrics at `/metrics`:

- `github_rate_limit_remaining` - Number of requests remaining in current rate limit window
- `github_rate_limit_reset` - Unix timestamp when rate limit window resets
