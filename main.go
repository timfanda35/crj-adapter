package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := loadConfig()
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner, err := NewAdminRunner(ctx)
	if err != nil {
		log.Error("init runner", "err", err)
		os.Exit(1)
	}
	defer runner.Close()

	mux := http.NewServeMux()
	mux.Handle("/", newPushHandler(cfg, runner, log))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:              ":" + getenv("PORT", "8080"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("listening", "addr", srv.Addr, "project", cfg.Project, "default_region", cfg.DefaultRegion)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
}

func loadConfig() (Config, error) {
	cfg := Config{
		Project:       os.Getenv("PROJECT_ID"),
		DefaultRegion: os.Getenv("DEFAULT_REGION"),
	}
	if cfg.Project == "" {
		return cfg, errors.New("PROJECT_ID is required")
	}
	if cfg.DefaultRegion == "" {
		return cfg, errors.New("DEFAULT_REGION is required")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
