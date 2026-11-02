package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"how-is-the-weather-at-api/internal/api"
	"how-is-the-weather-at-api/internal/forecast"
	"how-is-the-weather-at-api/internal/metno"
)

type config struct {
	userAgent    string
	upstreamURL  string
	upstreamRate float64
	port         string
	cacheSize    int
	redisURL     string
	inMemory     bool
}

// Calls to Redis fail fast, so requests fail closed quickly when it is down.
const redisTimeout = 200 * time.Millisecond

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := run(cfg, logger); err != nil {
		logger.Error("service failed", slog.Any("error", err))
		os.Exit(1)
	}
}

func parseConfig(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("how-is-the-weather-at-api", flag.ContinueOnError)
	fs.StringVar(&cfg.userAgent, "user-agent", "", "User-Agent sent to met.no: application name and contact details (required by the met.no Terms of Service)")
	fs.StringVar(&cfg.upstreamURL, "upstream-url", metno.DefaultURL, "met.no Locationforecast compact endpoint")
	fs.Float64Var(&cfg.upstreamRate, "upstream-rate", 10, fmt.Sprintf("most met.no calls per second, at most %d", forecast.MaxRate))
	fs.StringVar(&cfg.port, "port", "", "port to listen on (default: $PORT, otherwise 8080)")
	fs.IntVar(&cfg.cacheSize, "cache-size", 10_000, "most locations kept in memory, with -in-memory")
	fs.StringVar(&cfg.redisURL, "redis-url", "", "Redis shared by all instances, e.g. redis://localhost:6379")
	fs.BoolVar(&cfg.inMemory, "in-memory", false, "keep the state in this process instead of Redis; for tests and local runs, single instance only")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	cfg.port = cmp.Or(cfg.port, os.Getenv("PORT"), "8080")
	switch p, err := strconv.Atoi(cfg.port); {
	case cfg.userAgent == "":
		return config{}, errors.New("-user-agent is required: the met.no Terms of Service require an application name and contact details, e.g. \"how-is-the-weather-at-api/1.0 you@example.com\"")
	case cfg.upstreamRate <= 0 || cfg.upstreamRate > forecast.MaxRate:
		return config{}, fmt.Errorf("-upstream-rate must be above 0 and at most %d, so a burst of saved-up calls plus a second of new ones stays within met.no's %d req/s",
			forecast.MaxRate, forecast.MetnoRateLimit)
	case err != nil || p < 1 || p > 65535:
		return config{}, fmt.Errorf("invalid port %q", cfg.port)
	case cfg.cacheSize < 1:
		return config{}, errors.New("-cache-size must be at least 1")
	case (cfg.redisURL == "") == !cfg.inMemory:
		return config{}, errors.New("give exactly one of -redis-url (shared state, any number of instances) and -in-memory (one instance, for tests and local runs)")
	}
	return cfg, nil
}

func run(cfg config, logger *slog.Logger) error {
	client, err := metno.NewClient(&http.Client{}, cfg.upstreamURL, cfg.userAgent)
	if err != nil {
		return err
	}

	store, closeStore, err := openStore(cfg, logger)
	if err != nil {
		return err
	}
	defer closeStore()

	// met.no calls run on this context, so shutdown can cancel them.
	upstreamCtx, cancelUpstream := context.WithCancel(context.Background())
	defer cancelUpstream()
	cache := forecast.New(upstreamCtx, forecast.Config{Fetcher: client, Store: store, Logger: logger})
	handler := api.New(cache, logger)

	srv := &http.Server{
		Addr:              net.JoinHostPort("", cfg.port),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	logger.Info("listening", slog.String("addr", srv.Addr), slog.Float64("upstream_rate", cfg.upstreamRate))

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	// Stop taking requests and let running ones finish, then stop met.no calls and wait for them.
	logger.Info("shutting down")
	handler.ShuttingDown()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	cancelUpstream()
	cache.Wait()
	return err
}

func openStore(cfg config, logger *slog.Logger) (forecast.Store, func(), error) {
	if cfg.inMemory {
		logger.Warn("keeping state in memory: run only one instance, or the met.no rate limit no longer holds")
		store, err := forecast.NewMemoryStore(cfg.upstreamRate, cfg.cacheSize)
		return store, func() {}, err
	}
	opts, err := redis.ParseURL(cfg.redisURL)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid -redis-url: %w", err)
	}
	opts.DialTimeout, opts.ReadTimeout, opts.WriteTimeout = redisTimeout, redisTimeout, redisTimeout
	opts.MaxRetries = -1
	rdb := redis.NewClient(opts)
	// Start even if Redis is down: /readyz fails until it can be reached, and requests fail closed.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		logger.Error("Redis is unreachable; failing closed until it is back", slog.Any("error", err))
	}
	return forecast.NewRedisStore(rdb, cfg.upstreamRate), func() { _ = rdb.Close() }, nil
}
