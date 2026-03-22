package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"museum/internal/database"
	"museum/internal/env"
	"museum/internal/repository"
	"museum/pkg/graceful"
)

func main() {
	env.LoadEnv()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, cancel := graceful.Context(context.Background())
	defer cancel()

	cfg := database.ConfigFromEnv()
	pool, err := database.NewPool(ctx, cfg)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	// Run migrations from the filesystem.
	// Wrap os.DirFS so that database.RunMigrations can read from "migrations".
	migrationsDir := os.Getenv("MIGRATIONS_DIR")
	if migrationsDir == "" {
		migrationsDir = "."
	}
	migrationsFS := os.DirFS(migrationsDir).(database.MigrationsFS)
	if err := database.RunMigrations(ctx, pool, migrationsFS, "up"); err != nil {
		slog.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}

	museumRepo := repository.NewMuseumRepository(pool)
	exhibitionRepo := repository.NewExhibitionRepository(pool)

	handler := NewHandler(museumRepo, exhibitionRepo)
	mux := http.NewServeMux()

	// Museum endpoints
	mux.HandleFunc("GET /api/v1/museums", handler.ListMuseums)
	mux.HandleFunc("GET /api/v1/museums/{id}", handler.GetMuseum)
	mux.HandleFunc("GET /api/v1/museums/search", handler.SearchMuseums)
	mux.HandleFunc("GET /api/v1/museums/nearby", handler.NearbyMuseums)
	mux.HandleFunc("GET /api/v1/museums/city/{city}", handler.MuseumsByCity)
	mux.HandleFunc("GET /api/v1/museums/country/{country}", handler.MuseumsByCountry)

	// Exhibition endpoints
	mux.HandleFunc("GET /api/v1/exhibitions/city/{city}", handler.ExhibitionsByCity)
	mux.HandleFunc("GET /api/v1/exhibitions/nearby", handler.ExhibitionsNearby)

	// Health check
	mux.HandleFunc("GET /health", handler.HealthCheck)

	port := os.Getenv("API_PORT")
	if port == "" {
		port = "8081"
	}

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      withMiddleware(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		slog.Info("starting API server", "port", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down server...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("server shutdown error", "error", err)
	}
}
