package graceful

import (
	"context"
	"errors"
	"log"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestGracefulContext(t *testing.T) {
	// We need to create a pipe to temporarily redirect log output during the test
	// to avoid "Received termination signal" from printing to the console.
	r, w, _ := os.Pipe()
	oldStdout := os.Stdout
	os.Stdout = w

	// Create a context that will be canceled on signal.
	ctx, cancel := Context(context.Background())
	defer cancel()

	// Simulate sending an interrupt signal to the process.
	// This will trigger the signal handler in setupGracefulShutdown.
	go func() {
		time.Sleep(100 * time.Millisecond) // Give the signal handler time to get ready
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
			t.Errorf("Failed to send SIGINT: %v", err)
		}
	}()

	// Wait for the context to be canceled, with a timeout.
	select {
	case <-ctx.Done():
		// Context was canceled, which is the expected behavior.
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Errorf("Expected context.Canceled error, got %v", ctx.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Test timed out waiting for context to be canceled.")
	}

	// Restore log output.
	_ = w.Close()
	os.Stdout = oldStdout

	log.SetOutput(os.Stderr)
	// You can read from the pipe to check if the log message was written, but for
	// this test, we just want to ensure it doesn't cause a problem.
	_ = r.Close()
}
