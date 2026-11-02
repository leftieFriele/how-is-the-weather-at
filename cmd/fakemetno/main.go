package main

import (
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"how-is-the-weather-at-api/internal/fakemetno"
)

func main() {
	port := flag.String("port", "8081", "port to listen on")
	expiresIn := flag.Duration("expires-in", fakemetno.DefaultExpiresIn, "how far ahead the Expires header is")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fake := fakemetno.New()
	fake.SetExpiresIn(*expiresIn)

	srv := &http.Server{
		Addr: ":" + *port,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger.Info("request", slog.String("query", r.URL.RawQuery), slog.String("user_agent", r.UserAgent()))
			fake.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	logger.Info("fake met.no listening", slog.String("addr", srv.Addr))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", slog.Any("error", err))
		os.Exit(1)
	}
}
