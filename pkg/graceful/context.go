package graceful

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
)

// Context creates a context that is canceled when an OS interrupt signal is received.
// This allows for a clean shutdown of the application.
func Context(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Received termination signal, starting graceful shutdown...")
		cancel()
	}()

	return ctx, cancel
}
