package database

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MigrationsFS is the interface required for reading migration files.
// Both embed.FS and os.DirFS satisfy this interface.
type MigrationsFS interface {
	fs.ReadDirFS
	fs.ReadFileFS
}

// RunMigrations executes SQL migration files against the database.
// The migrationsFS should contain a "migrations" subdirectory with .up.sql / .down.sql files.
// Direction should be "up" or "down".
func RunMigrations(ctx context.Context, pool *pgxpool.Pool, migrationsFS MigrationsFS, direction string) error {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	suffix := "." + direction + ".sql"
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			files = append(files, e.Name())
		}
	}

	if direction == "down" {
		sort.Sort(sort.Reverse(sort.StringSlice(files)))
	} else {
		sort.Strings(files)
	}

	for _, f := range files {
		data, err := migrationsFS.ReadFile("migrations/" + f)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", f, err)
		}
		slog.Info("running migration", "file", f, "direction", direction)
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			return fmt.Errorf("exec migration %s: %w", f, err)
		}
		slog.Info("migration complete", "file", f)
	}
	return nil
}
