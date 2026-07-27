package command

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"museum/internal/api"
	"museum/pkg/graceful"
)

// serveCommand runs the HTTP API.
func serveCommand() Command {
	return Command{
		Name:    "serve",
		Summary: "Run the HTTP API",
		Usage:   "[-addr :8090]",
		Run:     runServe,
	}
}

func runServe(ctx context.Context, args []string) error {
	fs := newFlagSet("serve", "[-addr :8090]", os.Stderr)
	var (
		addr        = fs.String("addr", ":8090", "address to listen on")
		readTimeout = fs.Duration("read-timeout", 10*time.Second, "per-request read timeout")
		idleTimeout = fs.Duration("idle-timeout", 60*time.Second, "keep-alive idle timeout")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArgs("serve", fs.Args()); err != nil {
		return err
	}

	db, err := database(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	server := &http.Server{
		Addr:              *addr,
		Handler:           api.NewServer(db).Routes(),
		ReadHeaderTimeout: *readTimeout,
		ReadTimeout:       *readTimeout,
		IdleTimeout:       *idleTimeout,
	}

	ctx, cancel := graceful.Context(ctx)
	defer cancel()

	failed := make(chan error, 1)
	go func() {
		log.Printf("Listening on %s", *addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- fmt.Errorf("serve: %w", err)
			return
		}
		failed <- nil
	}()

	select {
	case err := <-failed:
		// The listener never came up — a bound port, most likely — so there is
		// nothing to shut down gracefully.
		return err
	case <-ctx.Done():
	}

	// Shutdown gets a fresh context: the signal that stopped the server also
	// cancelled ctx, and reusing it would abort in-flight requests immediately
	// rather than letting them finish.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelShutdown()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	log.Println("Server stopped")
	return nil
}
