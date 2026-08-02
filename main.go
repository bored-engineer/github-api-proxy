package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"path"
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
	"github.com/rs/xid"
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
	zerolog.DurationFieldInteger = true

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	apiURL := pflag.String("url", "https://api.github.com/", "GitHub API URL")
	listenAddrs := pflag.StringSlice("listen", []string{"127.0.0.1:44879"}, "Address(es) to listen on, optionally prefixed with 'network:' (e.g. 'unix:/github-api-proxy.sock'); network defaults to 'tcp'")
	tlsCert := pflag.String("tls-cert", "", "TLS certificate file to use")
	tlsKey := pflag.String("tls-key", "", "TLS key file to use")
	cacheMemory := pflag.Bool("cache-memory", false, "Use an in-memory cache for conditional requests (lost on restart, not shared across processes)")
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
	authApp := pflag.StringSlice("auth-app", nil, "GitHub App clients for GitHub API authentication in the format 'client_id:installation_id:private_key'")
	authToken := pflag.StringSlice("auth-token", nil, "GitHub personal access tokens for GitHub API authentication")
	rate := pflag.Int("rate", 0, "maximum requests per hour, shared globally across all authentication tokens")
	rateInterval := pflag.Duration("rate-interval", 60*time.Second, "Interval for rate limit checks")
	rateResources := pflag.StringSlice("rate-resources", []string{"core", "graphql"}, "Resource types to report rate limit metrics for (empty means report all)")
	rateReserve := pflag.Bool("rate-reserve", true, "Proactively reserve rate limit capacity for in-flight requests before response headers are parsed")
	rateSpoof := pflag.Bool("rate-spoof", true, "Return a synthetic 403 response instead of forwarding requests once a credential's rate limit is exhausted")
	srcIPs := pflag.StringSlice("src-ip", nil, "Source IP addresses to balance outgoing requests across (round-robin)")
	logLevel := pflag.String("log-level", "info", "minimum log level to output (trace, debug, info, warn, error)")
	logRateLimit := pflag.Bool("log-rate-limit", false, "log requests to the /rate_limit API, normally suppressed since they're issued periodically to poll rate limit status rather than in response to real traffic")
	logRequestLocalAddr := pflag.Bool("log-request-local-addr", true, "log the listener's own address (the one the client connected to) as request.conn.local")
	logRequestRemoteAddr := pflag.Bool("log-request-remote-addr", true, "log the client's address, plus the incoming connection's id, as request.conn.remote")
	logResponseLocalAddr := pflag.Bool("log-response-local-addr", true, "log the address the proxy dialed out from (e.g. matching a configured --src-ip) as response.conn.local")
	logResponseRemoteAddr := pflag.Bool("log-response-remote-addr", true, "log the upstream GitHub server's address, plus the connection's id, as response.conn.remote")
	logResponseConnReuse := pflag.Bool("log-response-conn-reuse", true, "log whether the upstream connection used for each request was freshly dialed or reused from the pool (response.conn.remote.reused, response.conn.remote.was_idle, response.conn.remote.idle_time)")
	logRequestHeaders := pflag.Bool("log-request-headers", false, "log the full request headers for each request (the Authorization value, if any, is always replaced with its hash)")
	logResponseHeaders := pflag.Bool("log-response-headers", false, "log the full response headers for each request (the Authorization value, if any, is always replaced with its hash)")
	internalPrefix := pflag.String("internal-prefix", "/github-api-proxy/", "URL path prefix under which internal endpoints (pprof, metrics, rate_limits) are served")
	pprofEnabled := pflag.Bool("pprof", false, "expose net/http/pprof debug endpoints under <internal-prefix>/pprof/ (WARNING: allows dumping goroutines, heap, and CPU profiles; do not enable on a publicly reachable listener)")
	metricsEnabled := pflag.Bool("metrics", true, "expose Prometheus metrics under <internal-prefix>/metrics")
	rateLimitsEnabled := pflag.Bool("rate-limits", false, "expose <internal-prefix>/rate_limits, which live-polls /rate_limit for every configured transport in parallel and returns the aggregated results as JSON")
	pflag.Parse()

	prefix := path.Join("/", *internalPrefix)

	level, err := zerolog.ParseLevel(*logLevel)
	if err != nil {
		log.Fatal().Err(err).Str("log_level", *logLevel).Msg("zerolog.ParseLevel failed")
	}
	zerolog.SetGlobalLevel(level)

	proxyURL, err := url.Parse(*apiURL)
	if err != nil {
		log.Fatal().Err(err).Msg("url.Parse failed")
	}
	rateLimitURL := proxyURL.ResolveReference(&url.URL{Path: "/rate_limit"})

	// Build one dedicated transport per source IP (or just the default
	// transport if none were provided) to balance outgoing connections.
	srcIPDials, err := SrcIPTransports(*srcIPs, http.DefaultTransport)
	if err != nil {
		log.Fatal().Err(err).Msg("SrcIPTransports failed")
	}

	// Setup the relevant storage backend, if any (caching is disabled unless
	// one of --cache-memory or --cache-* is given).
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
	} else if *cacheMemory {
		storage = memory.NewStorage()
	}

	// Wrap each source IP's dial transport with status tracking (before) and
	// conditional-request caching (after; storage may be nil if no backend
	// was configured, in which case nothing is read/written but the
	// speculative empty-array ETag guess still applies), then wrap the whole
	// thing with logging, so it can report both the status the caching layer
	// returns and (via context) the raw upstream status, giving one full
	// chain per source IP.
	chains := make([]http.RoundTripper, len(srcIPDials))
	for idx, dial := range srcIPDials {
		base := &LatencyTransport{Base: dial}
		chains[idx] = &LoggingTransport{
			Base:                  ghtransport.NewTransport(storage, base),
			LogRateLimit:          *logRateLimit,
			LogRequestLocalAddr:   *logRequestLocalAddr,
			LogRequestRemoteAddr:  *logRequestRemoteAddr,
			LogResponseLocalAddr:  *logResponseLocalAddr,
			LogResponseRemoteAddr: *logResponseRemoteAddr,
			LogResponseConnReuse:  *logResponseConnReuse,
			LogRequestHeaders:     *logRequestHeaders,
			LogResponseHeaders:    *logResponseHeaders,
		}
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
	var rateLimitSources []RateLimitSource
	if len(*authOAuth) > 0 || len(*authApp) > 0 || len(*authToken) > 0 {
		var balancing ghratelimit.BalancingTransport
		// If using OAuth credentials, just use basic auth.
		for _, params := range *authOAuth {
			clientID, clientSecret, ok := strings.Cut(params, ":")
			if !ok {
				log.Fatal().Str("params", params).Msg("invalid OAuth client")
			}
			authTransport, err := ghauth.Basic(&ContextTransport{
				Base:  newBaseTransport(),
				Key:   ctxAuthClientID{},
				Value: clientID,
			}, clientID, clientSecret)
			if err != nil {
				log.Fatal().Err(err).Str("client_id", clientID).Msg("ghauth.Basic failed")
			}
			t := &ghratelimit.Transport{
				Base: authTransport,
				Limits: ghratelimit.Limits{
					Notify: reportRateLimit(clientID, "", "", *rateResources),
				},
				Reserve: *rateReserve,
				Spoof:   *rateSpoof,
			}
			balancing = append(balancing, t)
			rateLimitSources = append(rateLimitSources, RateLimitSource{ClientID: clientID, Transport: t})
		}
		// If using GitHub App credentials, use the GitHub App transport.
		for _, appParams := range *authApp {
			clientID, appParams, ok := strings.Cut(appParams, ":")
			if !ok {
				log.Fatal().Str("params", appParams).Msg("invalid GitHub App")
			}
			installationID, privateKey, ok := strings.Cut(appParams, ":")
			if !ok {
				log.Fatal().Str("params", appParams).Msg("invalid GitHub App")
			}
			ts, err := ghauth.App(ctx, clientID, installationID, privateKey)
			if err != nil {
				log.Fatal().Err(err).Str("client_id", clientID).Msg("ghauth.App failed")
			}
			t := &ghratelimit.Transport{
				Base: &oauth2.Transport{
					Base: &ContextTransport{
						Base: &ContextTransport{
							Base:  newBaseTransport(),
							Key:   ctxAuthClientID{},
							Value: clientID,
						},
						Key:   ctxAuthInstallationID{},
						Value: installationID,
					},
					Source: ts,
				},
				Limits: ghratelimit.Limits{
					Notify: reportRateLimit(clientID, installationID, "", *rateResources),
				},
				Reserve: *rateReserve,
				Spoof:   *rateSpoof,
			}
			balancing = append(balancing, t)
			rateLimitSources = append(rateLimitSources, RateLimitSource{ClientID: clientID, InstallationID: installationID, Transport: t})
		}
		for _, token := range *authToken {
			hashed := sha256.Sum256([]byte(token))
			hashedToken := base64.StdEncoding.EncodeToString(hashed[:])
			t := &ghratelimit.Transport{
				Base: &oauth2.Transport{
					Base:   newBaseTransport(),
					Source: oauth2.StaticTokenSource(ghauth.Token(token)),
				},
				Limits: ghratelimit.Limits{
					Notify: reportRateLimit("", "", hashedToken, *rateResources),
				},
				Reserve: *rateReserve,
				Spoof:   *rateSpoof,
			}
			balancing = append(balancing, t)
			rateLimitSources = append(rateLimitSources, RateLimitSource{HashedToken: hashedToken, Transport: t})
		}
		// Poll the rate limits for each transport.
		go balancing.Poll(ctx, *rateInterval, rateLimitURL)
		transport = balancing
	}

	// If --rate is set, apply a single rate limit shared across all
	// credentials/source IPs (rather than one budget per credential), so
	// the configured throughput is a global cap on the proxy as a whole.
	if *rate > 0 {
		transport = ratelimit.New(transport, *rate, ratelimit.Per(time.Hour))
	}

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
	mux.Handle("/api/v3/", http.StripPrefix("/api/v3/", proxy))
	if *metricsEnabled {
		mux.Handle(path.Join(prefix, "metrics"), promhttp.Handler())
	}
	if *rateLimitsEnabled {
		mux.Handle(path.Join(prefix, "rate_limits"), RateLimitsHandler(rateLimitSources, rateLimitURL))
	}
	if *pprofEnabled {
		pprofPrefix := path.Join(prefix, "pprof") + "/"
		// net/http/pprof's own Index only recognizes named profiles
		// (e.g. "heap") under a literal "/debug/pprof/" prefix, so route
		// named lookups through pprof.Handler directly rather than relying
		// on Index's internal routing, which would never match under pprofPrefix.
		mux.HandleFunc(pprofPrefix, func(w http.ResponseWriter, r *http.Request) {
			if name := strings.TrimPrefix(r.URL.Path, pprofPrefix); name != "" {
				pprof.Handler(name).ServeHTTP(w, r)
				return
			}
			pprof.Index(w, r)
		})
		mux.HandleFunc(pprofPrefix+"cmdline", pprof.Cmdline)
		mux.HandleFunc(pprofPrefix+"profile", pprof.Profile)
		mux.HandleFunc(pprofPrefix+"symbol", pprof.Symbol)
		mux.HandleFunc(pprofPrefix+"trace", pprof.Trace)
	}

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
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxXID{}, xid.New())))
		}),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			ctx = context.WithValue(ctx, ctxConnXID{}, xid.New())
			ctx = context.WithValue(ctx, ctxConnLocalAddr{}, c.LocalAddr().String())
			return ctx
		},
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
