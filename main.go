package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
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
	"github.com/bored-engineer/ratelimit-transport"
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
		[]string{"client_id", "resource"},
	)
	RateLimitReset = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "rate_limit_reset",
			Help:      "Unix timestamp when the current rate limit window resets",
			Subsystem: "github",
		},
		[]string{"client_id", "resource"},
	)

	// Abstract ghauth functions for easier testing
	ghauthBasic = func(base http.RoundTripper, clientID, clientSecret string) (http.RoundTripper, error) {
		return ghauth.Basic(base, clientID, clientSecret)
	}
	ghauthApp = func(ctx context.Context, appID, installationID, privateKey string) (oauth2.TokenSource, error) {
		return ghauth.App(ctx, appID, installationID, privateKey)
	}
)

func main() {
	// Initialize zerolog
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	apiURL := pflag.String("url", "https://api.github.com/", "GitHub API URL")
	listenAddr := pflag.String("listen", "127.0.0.1:44879", "Address to listen on")
	tlsCert := pflag.String("tls-cert", "", "TLS certificate file to use")
	tlsKey := pflag.String("tls-key", "", "TLS key file to use")
	pebbleDBPath := pflag.String("pebble-db", "", "Path to PebbleDB to use for caching")
	boltDBPath := pflag.String("bbolt-db", "", "Path to BoltDB to use for caching")
	boltDBBucket := pflag.String("bbolt-bucket", "github-api-proxy", "BoltDB bucket to use for caching")
	s3Bucket := pflag.String("s3-bucket", "", "S3 bucket to use")
	s3Region := pflag.String("s3-region", "", "S3 region to use")
	s3Endpoint := pflag.String("s3-endpoint", "", "S3 endpoint to use")
	s3Prefix := pflag.String("s3-prefix", "", "S3 prefix to use")
	redisAddr := pflag.String("redis-addr", "", "Redis address to use")
	redisUsername := pflag.String("redis-username", "", "Redis username to use")
	redisPassword := pflag.String("redis-password", "", "Redis password to use")
	redisDB := pflag.Int("redis-db", 0, "Redis database to use")
	authOAuth := pflag.StringSlice("auth-oauth", nil, "OAuth clients for GitHub API authentication in the format 'client_id:client_secret'")
	authApp := pflag.StringSlice("auth-app", nil, "GitHub App clients for GitHub API authentication in the format 'app_id:installation_id:private_key'")
	authToken := pflag.StringSlice("auth-token", nil, "GitHub personal access tokens for GitHub API authentication with an optional identifier for use in metrics labeling in the format 'token' or 'identifier:token'")
	rph := pflag.Int("rph", 0, "maximum requests per hour (per authentication token)")
	rateInterval := pflag.Duration("rate-interval", 60*time.Second, "Interval for rate limit checks")
	pflag.Parse()

	proxyURL, err := url.Parse(*apiURL)
	if err != nil {
		log.Fatal().Err(err).Msg("url.Parse failed")
	}

	// Set up the relevant storage backend, defaulting to in-memory.
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

	// Implement the logging _before_ the caching
	var transport http.RoundTripper = &LoggingTransport{
		Base: http.DefaultTransport,
	}

	// Set up the caching transport as the base transport.
	transport = ghtransport.NewTransport(storage, transport)

	// If credentials were provided, balancing requests across them.
	if len(*authOAuth) > 0 || len(*authApp) > 0 || len(*authToken) > 0 {
		var balancing ghratelimit.BalancingTransport
		// If using OAuth credentials, just use basic auth.
		for _, params := range *authOAuth {
			balancing = configureOauthTransport(params, transport, balancing)
		}
		// If using GitHub App credentials, use the GitHub App transport.
		for _, appParams := range *authApp {
			balancing = configureGitHubApp(ctx, appParams, transport, balancing)
		}
		for _, token := range *authToken {
			balancing = configurePatTransport(token, transport, balancing)
		}
		// Poll the rate limits for each transport.
		go balancing.Poll(ctx, *rateInterval, proxyURL.ResolveReference(&url.URL{
			Path: "/rate_limit",
		}))
		transport = balancing
	} else {
		// If RPH is set, wrap the main transport in a rate-limiting transport.
		if *rph > 0 {
			transport = ratelimit.New(transport, *rph, ratelimit.Per(time.Hour))
		}
	}

	// Set up the reverse proxy.
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

	// Set up the HTTP router.
	mux := http.NewServeMux()
	mux.Handle("/", proxy)
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/api/v3/", http.StripPrefix("/api/v3/", proxy))

	// Start the HTTP server.
	server := &http.Server{
		Addr:    *listenAddr,
		Handler: mux,
	}
	go func() {
		if *tlsCert != "" && *tlsKey != "" {
			if err := server.ListenAndServeTLS(*tlsCert, *tlsKey); !errors.Is(err, http.ErrServerClosed) {
				log.Fatal().Err(err).Msg("(*http.Server).ListenAndServeTLS failed")
			}
		} else {
			if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				log.Fatal().Err(err).Msg("(*http.Server).ListenAndServe failed")
			}
		}
	}()

	// When an interrupt is received, gracefully shut down the HTTP server.
	<-ctx.Done()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatal().Err(err).Msg("(*http.Server).Shutdown failed")
	}

}

func configureGitHubApp(ctx context.Context, appParams string, transport http.RoundTripper, balancing ghratelimit.BalancingTransport) ghratelimit.BalancingTransport {
	appID, appParams, ok := strings.Cut(appParams, ":")
	if !ok {
		log.Fatal().Str("params", appParams).Msg("invalid GitHub App")
	}
	installationID, privateKey, ok := strings.Cut(appParams, ":")
	if !ok {
		log.Fatal().Str("params", appParams).Msg("invalid GitHub App")
	}
	ts, err := ghauthApp(ctx, appID, installationID, privateKey)
	if err != nil {
		log.Fatal().Err(err).Str("app_id", appID).Msg("ghauth.App failed")
	}
	balancing = append(balancing, &ghratelimit.Transport{
		Base: &oauth2.Transport{
			Base:   transport,
			Source: ts,
		},
		Limits: ghratelimit.Limits{
			Notify: func(resp *http.Response, resource ghratelimit.Resource, rate *ghratelimit.Rate) {
				RateLimitRemaining.WithLabelValues(appID+":"+installationID, resource.String()).Set(float64(rate.Remaining))
				RateLimitReset.WithLabelValues(appID+":"+installationID, resource.String()).Set(float64(rate.Reset))
			},
		},
	})
	return balancing
}

func configureOauthTransport(params string, transport http.RoundTripper, balancing ghratelimit.BalancingTransport) ghratelimit.BalancingTransport {
	clientID, clientSecret, ok := strings.Cut(params, ":")
	if !ok {
		log.Fatal().Str("params", params).Msg("invalid OAuth client")
	}
	authTransport, err := ghauthBasic(transport, clientID, clientSecret)
	if err != nil {
		log.Fatal().Err(err).Str("client_id", clientID).Msg("ghauth.Basic failed")
	}
	balancing = append(balancing, &ghratelimit.Transport{
		Base: authTransport,
		Limits: ghratelimit.Limits{
			Notify: func(resp *http.Response, resource ghratelimit.Resource, rate *ghratelimit.Rate) {
				RateLimitRemaining.WithLabelValues(clientID, resource.String()).Set(float64(rate.Remaining))
				RateLimitReset.WithLabelValues(clientID, resource.String()).Set(float64(rate.Reset))
			},
		},
	})
	return balancing
}

func configurePatTransport(token string, transport http.RoundTripper, balancing ghratelimit.BalancingTransport) ghratelimit.BalancingTransport {
	tokenId, token, ok := strings.Cut(token, ":")
	if !ok {
		token = tokenId
		hashed := sha256.Sum256([]byte(token))
		tokenId = base64.StdEncoding.EncodeToString(hashed[:])
	}
	balancing = append(balancing, &ghratelimit.Transport{
		Base: &oauth2.Transport{
			Base:   transport,
			Source: oauth2.StaticTokenSource(ghauth.Token(token)),
		},
		Limits: ghratelimit.Limits{
			Notify: func(resp *http.Response, resource ghratelimit.Resource, rate *ghratelimit.Rate) {
				RateLimitRemaining.WithLabelValues(tokenId, resource.String()).Set(float64(rate.Remaining))
				RateLimitReset.WithLabelValues(tokenId, resource.String()).Set(float64(rate.Reset))
			},
		},
	})
	return balancing
}
