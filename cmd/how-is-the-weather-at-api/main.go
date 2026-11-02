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

	"how-is-the-weather-at-api/internal/api"
	"how-is-the-weather-at-api/internal/forecast"
	"how-is-the-weather-at-api/internal/metno"
)

// met.no's limit per application, shared by all our environments.
const maxUpstreamRate = 20

type config struct {
	userAgent    string
	upstreamURL  string
	upstreamRate float64
	port         string
	cacheSize    int
}

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
	fs.Float64Var(&cfg.upstreamRate, "upstream-rate", 10, "most met.no calls per second, at most 20")
	fs.StringVar(&cfg.port, "port", "", "port to listen on (default: $PORT, otherwise 8080)")
	fs.IntVar(&cfg.cacheSize, "cache-size", 10_000, "most locations kept in the forecast cache")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	cfg.port = cmp.Or(cfg.port, os.Getenv("PORT"), "8080")
	switch p, err := strconv.Atoi(cfg.port); {
	case cfg.userAgent == "":
		return config{}, errors.New("-user-agent is required: the met.no Terms of Service require an application name and contact details, e.g. \"how-is-the-weather-at-api/1.0 you@example.com\"")
	case cfg.upstreamRate <= 0 || cfg.upstreamRate > maxUpstreamRate:
		return config{}, fmt.Errorf("-upstream-rate must be above 0 and at most %d (met.no's limit per application)", maxUpstreamRate)
	case err != nil || p < 1 || p > 65535:
		return config{}, fmt.Errorf("invalid port %q", cfg.port)
	case cfg.cacheSize < 1:
		return config{}, errors.New("-cache-size must be at least 1")
	}
	return cfg, nil
}

func run(cfg config, logger *slog.Logger) error {
	client, err := metno.NewClient(&http.Client{}, cfg.upstreamURL, cfg.userAgent)
	if err != nil {
		return err
	}

	// met.no calls run on this context, so shutdown can cancel them.
	upstreamCtx, cancelUpstream := context.WithCancel(context.Background())
	defer cancelUpstream()
	cache, err := forecast.New(upstreamCtx, forecast.Config{
		Fetcher: client,
		Budget:  forecast.NewBudget(cfg.upstreamRate),
		Size:    cfg.cacheSize,
		Logger:  logger,
	})
	if err != nil {
		return err
	}
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
