package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	ghauth "github.com/bored-engineer/github-auth-http-transport"
	ghtransport "github.com/bored-engineer/github-conditional-http-transport"
	bboltstorage "github.com/bored-engineer/github-conditional-http-transport/bbolt"
	"github.com/bored-engineer/github-conditional-http-transport/memory"
	pebblestorage "github.com/bored-engineer/github-conditional-http-transport/pebble"
	redisstorage "github.com/bored-engineer/github-conditional-http-transport/redis"
	s3storage "github.com/bored-engineer/github-conditional-http-transport/s3"
	ghratelimit "github.com/bored-engineer/github-rate-limit-http-transport"
	ratelimit "github.com/bored-engineer/ratelimit-transport"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/pflag"
	"golang.org/x/oauth2"
)

var (
	// Register Prometheus metrics
	RateLimitRemaining = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "rate_limit_remaining",
			Help:      "Number of requests remaining in the current rate limit window",
			Subsystem: "github",
		},
		[]string{"client_id", "installation_id", "hashed_token", "resource"},
	)
	RateLimitReset = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "rate_limit_reset",
			Help:      "Unix timestamp when the current rate limit window resets",
			Subsystem: "github",
		},
		[]string{"client_id", "installation_id", "hashed_token", "resource"},
	)
)

// listenNetworks are the net.Listen network types recognized as an explicit
// "network:" prefix on a --listen value; anything else is treated as a plain
// address with no prefix, defaulting to "tcp".
var listenNetworks = []string{"tcp", "tcp4", "tcp6", "unix", "unixpacket"}

// splitListenAddr splits a --listen value of the form "[network:]address"
// into its network and address parts, defaulting network to "tcp" if no
// recognized network prefix is present (e.g. "127.0.0.1:44879" is a bare
// address, not a "127.0.0.1" network).
func splitListenAddr(s string) (network, address string) {
	if network, address, ok := strings.Cut(s, ":"); ok && slices.Contains(listenNetworks, network) {
		return network, address
	}
	return "tcp", s
}

// reportRateLimit returns a ghratelimit.Limits.Notify function that records the
// given rate limit as Prometheus metrics, labeled with clientID, installationID,
// and hashedToken (whichever apply to the credential type; others left as "").
// If resourceTypes is non-empty, only resources in that list are reported.
func reportRateLimit(clientID, installationID, hashedToken string, resourceTypes []string) func(*http.Response, ghratelimit.Resource, *ghratelimit.Rate) {
	return func(resp *http.Response, resource ghratelimit.Resource, rate *ghratelimit.Rate) {
		if len(resourceTypes) > 0 && !slices.Contains(resourceTypes, resource.String()) {
			return
		}
		RateLimitRemaining.WithLabelValues(clientID, installationID, hashedToken, resource.String()).Set(float64(rate.Remaining))
		RateLimitReset.WithLabelValues(clientID, installationID, hashedToken, resource.String()).Set(float64(rate.Reset))
	}
}

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	apiURL := pflag.String("url", "https://api.github.com/", "GitHub API URL")
	listenAddrs := pflag.StringSlice("listen", []string{"127.0.0.1:44879"}, "Address(es) to listen on, optionally prefixed with 'network:' (e.g. 'unix:/github-api-proxy.sock'); network defaults to 'tcp'")
	tlsCert := pflag.String("tls-cert", "", "TLS certificate file to use")
	tlsKey := pflag.String("tls-key", "", "TLS key file to use")
	pebbleDBPath := pflag.String("cache-pebble-db", "", "Path to PebbleDB to use for caching")
	boltDBPath := pflag.String("cache-bbolt-db", "", "Path to BoltDB to use for caching")
	boltDBBucket := pflag.String("cache-bbolt-bucket", "github-api-proxy", "BoltDB bucket to use for caching")
	s3Bucket := pflag.String("cache-s3-bucket", "", "S3 bucket to use")
	s3Region := pflag.String("cache-s3-region", "", "S3 region to use")
	s3Endpoint := pflag.String("cache-s3-endpoint", "", "S3 endpoint to use")
	s3Prefix := pflag.String("cache-s3-prefix", "", "S3 prefix to use")
	redisAddr := pflag.String("cache-redis-addr", "", "Redis address to use")
	redisUsername := pflag.String("cache-redis-username", "", "Redis username to use")
	redisPassword := pflag.String("cache-redis-password", "", "Redis password to use")
	redisDB := pflag.Int("cache-redis-db", 0, "Redis database to use")
	authOAuth := pflag.StringSlice("auth-oauth", nil, "OAuth clients for GitHub API authentication in the format 'client_id:client_secret'")
	authApp := pflag.StringSlice("auth-app", nil, "GitHub App clients for GitHub API authentication in the format 'app_id:installation_id:private_key'")
	authToken := pflag.StringSlice("auth-token", nil, "GitHub personal access tokens for GitHub API authentication")
	rph := pflag.Int("rph", 0, "maximum requests per hour, shared globally across all authentication tokens")
	rateInterval := pflag.Duration("rate-interval", 60*time.Second, "Interval for rate limit checks")
	rateResources := pflag.StringSlice("rate-resources", []string{"core", "graphql"}, "Resource types to report rate limit metrics for (empty means report all)")
	rateReserve := pflag.Bool("rate-reserve", true, "Proactively reserve rate limit capacity for in-flight requests before response headers are parsed")
	rateSpoof := pflag.Bool("rate-spoof", true, "Return a synthetic 429 response instead of forwarding requests once a credential's rate limit is exhausted")
	srcIPs := pflag.StringSlice("src-ip", nil, "Source IP addresses to balance outgoing requests across (round-robin)")
	retries := pflag.Int("retries", 2, "maximum retries for requests without a body that fail with a network error or a 429/5xx status code (0 disables retries)")
	retryWait := pflag.Duration("retry-wait", 250*time.Millisecond, "initial backoff delay between retries (doubles each attempt, subject to --retry-wait-max)")
	retryWaitMax := pflag.Duration("retry-wait-max", 30*time.Second, "maximum backoff delay between retries")
	rateRetry := pflag.Bool("rate-retry", true, "retry 429 responses until Retry-After clears, instead of counting them against --retries")
	logLevel := pflag.String("log-level", "info", "minimum log level to output (trace, debug, info, warn, error)")
	logFormat := pflag.String("log-format", "console", "log output format, 'console' for human-readable or 'json' for structured")
	logRateLimit := pflag.Bool("log-rate-limit", false, "log requests to the /rate_limit API, normally suppressed since they're issued periodically to poll rate limit status rather than in response to real traffic")
	logAddr := pflag.Bool("log-addr", false, "log the incoming/remote/source IP and port for each request")
	logRequestHeaders := pflag.Bool("log-request-headers", false, "log the full request headers for each request (Authorization/Cookie/Set-Cookie values are always redacted)")
	logResponseHeaders := pflag.Bool("log-response-headers", false, "log the full response headers for each request (Authorization/Cookie/Set-Cookie values are always redacted)")
	pflag.Parse()

	level, err := zerolog.ParseLevel(*logLevel)
	if err != nil {
		log.Fatal().Err(err).Str("log_level", *logLevel).Msg("zerolog.ParseLevel failed")
	}
	zerolog.SetGlobalLevel(level)
	switch *logFormat {
	case "console":
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	case "json":
		// This is zerolog's default output, nothing to do.
	default:
		log.Fatal().Str("log_format", *logFormat).Msg("invalid --log-format, must be 'console' or 'json'")
	}

	proxyURL, err := url.Parse(*apiURL)
	if err != nil {
		log.Fatal().Err(err).Msg("url.Parse failed")
	}

	// Build one dedicated transport per source IP (or just the default
	// transport if none were provided) to balance outgoing connections.
	srcIPDials, err := SrcIPTransports(*srcIPs, http.DefaultTransport)
	if err != nil {
		log.Fatal().Err(err).Msg("SrcIPTransports failed")
	}

	// Setup the relevant storage backend, defaulting to in-memory.
	var storage ghtransport.Storage
	if *pebbleDBPath != "" {
		pebbleStorage, err := pebblestorage.Open(*pebbleDBPath, nil)
		if err != nil {
			log.Fatal().Err(err).Msg("pebblestorage.Open failed")
		}
		defer func() {
			if err := pebbleStorage.DB.Close(); err != nil {
				log.Fatal().Err(err).Msg("(*pebble.DB).Close failed")
			}
		}()
		storage = pebbleStorage
	} else if *boltDBPath != "" {
		boltStorage, err := bboltstorage.Open(*boltDBPath, 0600, nil, []byte(*boltDBBucket))
		if err != nil {
			log.Fatal().Err(err).Msg("bboltstorage.Open failed")
		}
		defer func() {
			if err := boltStorage.DB.Close(); err != nil {
				log.Fatal().Err(err).Msg("(*bbolt.DB).Close failed")
			}
		}()
		storage = boltStorage
	} else if *s3Bucket != "" {
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(*s3Region))
		if err != nil {
			log.Fatal().Err(err).Msg("config.LoadDefaultConfig failed")
		}
		if *s3Region != "" {
			cfg.Region = *s3Region
		}
		s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
			if *s3Endpoint != "" {
				o.BaseEndpoint = aws.String(*s3Endpoint)
				// https://xuanwo.io/links/2025/02/aws_s3_sdk_breaks_its_compatible_services/
				o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
				o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
			}
		})
		s3Storage, err := s3storage.New(s3Client, *s3Bucket, *s3Prefix)
		if err != nil {
			log.Fatal().Err(err).Msg("s3storage.New failed")
		}
		storage = s3Storage
	} else if *redisAddr != "" {
		redisClient := redis.NewClient(&redis.Options{
			Addr:     *redisAddr,
			Username: *redisUsername,
			Password: *redisPassword,
			DB:       *redisDB,
		})
		storage = redisstorage.New(redisClient)
	} else {
		storage = memory.NewStorage()
	}

	// Wrap each source IP's dial transport with status tracking (before) and
	// caching (after), then wrap the whole thing with logging, so it can
	// report both the status the caching layer returns and (via context)
	// the raw upstream status, giving one full chain per source IP.
	chains := make([]http.RoundTripper, len(srcIPDials))
	for i, dial := range srcIPDials {
		loggingTransport := &LoggingTransport{
			Base:               ghtransport.NewTransport(storage, &UpstreamStatusTransport{Base: dial}),
			LogRateLimit:       *logRateLimit,
			LogAddr:            *logAddr,
			LogRequestHeaders:  *logRequestHeaders,
			LogResponseHeaders: *logResponseHeaders,
		}
		if len(*srcIPs) > 0 {
			loggingTransport.LocalIP = (*srcIPs)[i]
		}
		chains[i] = loggingTransport
	}

	// The default transport balances requests across all source IPs.
	var transport http.RoundTripper = RoundRobin(chains)

	// newBaseTransport returns a transport for a single authenticated
	// token, balanced round-robin per request across each source IP's
	// chain (or a single chain if no --src-ip was given).
	newBaseTransport := func() http.RoundTripper {
		return RoundRobin(chains)
	}

	// If credentials were provided, balancing requests across them.
	if len(*authOAuth) > 0 || len(*authApp) > 0 || len(*authToken) > 0 {
		var balancing ghratelimit.BalancingTransport
		// If using OAuth credentials, just use basic auth.
		for _, params := range *authOAuth {
			clientID, clientSecret, ok := strings.Cut(params, ":")
			if !ok {
				log.Fatal().Str("params", params).Msg("invalid OAuth client")
			}
			authTransport, err := ghauth.Basic(&AuthTransport{
				ClientID: clientID,
				Base:     newBaseTransport(),
			}, clientID, clientSecret)
			if err != nil {
				log.Fatal().Err(err).Str("client_id", clientID).Msg("ghauth.Basic failed")
			}
			balancing = append(balancing, &ghratelimit.Transport{
				Base: authTransport,
				Limits: ghratelimit.Limits{
					Notify: reportRateLimit(clientID, "", "", *rateResources),
				},
				Reserve: *rateReserve,
				Spoof:   *rateSpoof,
			})
		}
		// If using GitHub App credentials, use the GitHub App transport.
		for _, appParams := range *authApp {
			appID, appParams, ok := strings.Cut(appParams, ":")
			if !ok {
				log.Fatal().Str("params", appParams).Msg("invalid GitHub App")
			}
			installationID, privateKey, ok := strings.Cut(appParams, ":")
			if !ok {
				log.Fatal().Str("params", appParams).Msg("invalid GitHub App")
			}
			ts, err := ghauth.App(ctx, appID, installationID, privateKey)
			if err != nil {
				log.Fatal().Err(err).Str("app_id", appID).Msg("ghauth.App failed")
			}
			balancing = append(balancing, &ghratelimit.Transport{
				Base: &oauth2.Transport{
					Base: &AuthTransport{
						ClientID:       appID,
						InstallationID: installationID,
						Base:           newBaseTransport(),
					},
					Source: ts,
				},
				Limits: ghratelimit.Limits{
					Notify: reportRateLimit(appID, installationID, "", *rateResources),
				},
				Reserve: *rateReserve,
				Spoof:   *rateSpoof,
			})
		}
		for _, token := range *authToken {
			hashed := sha256.Sum256([]byte(token))
			hashedToken := base64.StdEncoding.EncodeToString(hashed[:])
			balancing = append(balancing, &ghratelimit.Transport{
				Base: &oauth2.Transport{
					Base:   &AuthTransport{Base: newBaseTransport()},
					Source: oauth2.StaticTokenSource(ghauth.Token(token)),
				},
				Limits: ghratelimit.Limits{
					Notify: reportRateLimit("", "", hashedToken, *rateResources),
				},
				Reserve: *rateReserve,
				Spoof:   *rateSpoof,
			})
		}
		// Poll the rate limits for each transport.
		go balancing.Poll(ctx, *rateInterval, proxyURL.ResolveReference(&url.URL{
			Path: "/rate_limit",
		}))
		transport = balancing
	}

	// If RPH is set, apply a single rate limit shared across all
	// credentials/source IPs (rather than one budget per credential), so
	// the configured throughput is a global cap on the proxy as a whole.
	if *rph > 0 {
		transport = ratelimit.New(transport, *rph, ratelimit.Per(time.Hour))
	}

	// Retry requests that fail with a network error or a 429/5xx status,
	// after balancing/rate-limiting so each attempt (including retries)
	// counts against the same global rate limit.
	if *retries > 0 || *rateRetry {
		transport = &RetryTransport{Base: transport, MaxRetries: *retries, RateRetry: *rateRetry, Wait: *retryWait, MaxWait: *retryWaitMax}
	}

	// Assign a unique ID to each incoming request, regardless of how the
	// transport chain above is configured, so retried attempts of the same
	// request (by RetryTransport) can be correlated across log lines.
	transport = &RequestIDTransport{Base: transport}

	// Setup the reverse proxy.
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(proxyURL)
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			// Replace the GitHub API URL with the proxy URL in the Link header.
			if link := resp.Header.Get("Link"); link != "" {
				resp.Header.Set("Link", strings.ReplaceAll(
					link,
					proxyURL.String(),
					resp.Request.Header.Get("X-Forwarded-Proto")+"://"+resp.Request.Header.Get("X-Forwarded-Host")+"/",
				))
			}
			return nil
		},
		Transport: transport,
	}

	// Setup the HTTP router.
	mux := http.NewServeMux()
	mux.Handle("/", proxy)
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/api/v3/", http.StripPrefix("/api/v3/", proxy))

	// Bind every requested listener up front so any invalid address is
	// reported immediately rather than after backgrounding the server.
	listeners := make([]net.Listener, len(*listenAddrs))
	for idx, listenAddr := range *listenAddrs {
		network, address := splitListenAddr(listenAddr)
		listener, err := net.Listen(network, address)
		if err != nil {
			log.Fatal().Err(err).Str("network", network).Str("address", address).Msg("net.Listen failed")
		}
		listeners[idx] = listener
	}

	// Start the HTTP server on each listener.
	server := &http.Server{
		Handler: mux,
	}
	for _, listener := range listeners {
		go func() {
			if *tlsCert != "" && *tlsKey != "" {
				if err := server.ServeTLS(listener, *tlsCert, *tlsKey); !errors.Is(err, http.ErrServerClosed) {
					log.Fatal().Err(err).Msg("(*http.Server).ServeTLS failed")
				}
			} else {
				if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
					log.Fatal().Err(err).Msg("(*http.Server).Serve failed")
				}
			}
		}()
	}

	// When an interrupt is received, gracefully shut down the HTTP server.
	<-ctx.Done()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatal().Err(err).Msg("(*http.Server).Shutdown failed")
	}

}
