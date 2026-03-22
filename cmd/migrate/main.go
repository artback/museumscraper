package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"museum/internal/database"
	"museum/internal/env"
)

func main() {
	direction := flag.String("direction", "up", "Migration direction: up or down")
	flag.Parse()

	if *direction != "up" && *direction != "down" {
		fmt.Fprintf(os.Stderr, "invalid direction: %s (must be 'up' or 'down')\n", *direction)
		os.Exit(1)
	}

	env.LoadEnv()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := database.ConfigFromEnv()
	pool, err := database.NewPool(ctx, cfg)
	if err != nil {
		slog.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	migrationsFS := os.DirFS(".").(database.MigrationsFS)
	if err := database.RunMigrations(ctx, pool, migrationsFS, *direction); err != nil {
		slog.Error("migration failed", "direction", *direction, "error", err)
		os.Exit(1)
	}

	slog.Info("migrations completed successfully", "direction", *direction)
}
