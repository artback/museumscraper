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
	"museum/pkg/location"
)

// drainTimeout is how long a stopping server waits for in-flight requests.
// Longer than the API's own per-request budget, so a request that is still
// within its deadline is allowed to finish rather than being cut off.
const drainTimeout = 20 * time.Second

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
		addr         = fs.String("addr", ":8090", "address to listen on")
		readTimeout  = fs.Duration("read-timeout", 10*time.Second, "per-request read timeout")
		writeTimeout = fs.Duration("write-timeout", 30*time.Second, "per-request write timeout")
		idleTimeout  = fs.Duration("idle-timeout", 60*time.Second, "keep-alive idle timeout")
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

	// The resolver is what lets a caller ask for "Paris" instead of a
	// coordinate pair. It geocodes through the shared rate-limited client and
	// caches into the same database, so a name costs one upstream call ever
	// rather than one per request.
	//
	// Scraping on demand is what makes a city nobody has looked at fill in when
	// someone does, rather than simply reading as empty.
	apiServer := api.NewServer(db).
		WithPlaces(api.NewPlaceResolver(db, location.Geocode)).
		WithScraping(db)
	defer apiServer.Close()

	server := &http.Server{
		Addr:              *addr,
		Handler:           apiServer.Routes(),
		ReadHeaderTimeout: *readTimeout,
		ReadTimeout:       *readTimeout,
		IdleTimeout:       *idleTimeout,
		// The backstop for a response that outlives its handler's deadline.
		// The handler timeout is the real bound; this one catches a client that
		// reads its body a byte at a time. Comfortably longer than the handler
		// budget so a slow-but-legitimate response is not cut off mid-encode.
		WriteTimeout: *writeTimeout,
		// Go's default is 1 MB of headers. Nothing this API accepts needs more
		// than a few hundred bytes, and the smaller ceiling costs an attacker
		// the cheapest way to make the server allocate.
		MaxHeaderBytes: 64 << 10,
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
	//
	// The budget must exceed the handler timeout, or the drain expires while
	// requests are still legitimately running: Shutdown returns "context
	// deadline exceeded", the command exits non-zero, and a clean SIGTERM looks
	// to the orchestrator like a crash. Worse, returning here runs the deferred
	// db.Close() and pulls the pool out from under the handlers still using it,
	// so every draining request fails — the opposite of graceful.
	shutdownCtx, cancelShutdown := context.WithTimeout(
		context.WithoutCancel(ctx), drainTimeout)
	defer cancelShutdown()

	if err := server.Shutdown(shutdownCtx); err != nil {
		// Report it, but keep going: the deferred close still has to run, and
		// the requests that did drain should not be undone by the ones that
		// did not.
		log.Printf("Shutdown timed out after %s, closing anyway: %v", drainTimeout, err)
		return nil
	}
	log.Println("Server stopped")
	return nil
}
