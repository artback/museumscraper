// Package command holds the museum tool's subcommands.
//
// Every subcommand is a separate process at runtime with its own lifecycle —
// serve and enrich run continuously, crawl and refresh are scheduled batches,
// reindex and query are run by hand — but they share one binary, one image and
// one set of connection settings. Splitting them into separate programs bought
// nothing: they never call each other, and coordinate entirely through keys in
// one object-storage bucket.
package command

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"museum/internal/env"
	"museum/internal/keys"
	"museum/internal/models"
	"museum/internal/postgres"
	"museum/internal/storage"
)

// Command is one subcommand of the museum tool.
type Command struct {
	// Name is the word used to invoke it.
	Name string
	// Summary is the one-line description shown by "museum help".
	Summary string
	// Usage is the argument synopsis, without the program name.
	Usage string
	// Run executes the command. args excludes the program and command names.
	Run func(ctx context.Context, args []string) error
}

// all is the registry, in the order help lists them: services first, then
// scheduled work, then the tools an operator reaches for.
func all() []Command {
	return []Command{
		serveCommand(),
		enrichCommand(),
		crawlCommand(),
		refreshCommand(),
		sweepCommand(),
		locateCommand(),
		reindexCommand(),
		verifyCommand(),
		queryCommand(),
	}
}

// Lookup finds a command by name.
func Lookup(name string) (Command, bool) {
	for _, cmd := range all() {
		if cmd.Name == name {
			return cmd, true
		}
	}
	return Command{}, false
}

// Names returns every command name, sorted.
func Names() []string {
	names := make([]string, 0, len(all()))
	for _, cmd := range all() {
		names = append(names, cmd.Name)
	}
	sort.Strings(names)
	return names
}

// Usage writes the top-level help.
func Usage(w io.Writer, program string) {
	fmt.Fprintf(w, "%s builds and serves a catalogue of the world's museums.\n\n", program)
	fmt.Fprintf(w, "Usage:\n  %s <command> [flags]\n\nCommands:\n", program)

	width := 0
	for _, cmd := range all() {
		width = max(width, len(cmd.Name))
	}
	for _, cmd := range all() {
		fmt.Fprintf(w, "  %-*s  %s\n", width, cmd.Name, cmd.Summary)
	}
	fmt.Fprintf(w, "\nRun \"%s <command> -h\" for the flags of one command.\n", program)
}

// newFlagSet builds a flag set that reports errors through Run's return value
// rather than exiting, and prints the command's own synopsis on -h.
func newFlagSet(cmd string, usage string, w io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(w)
	fs.Usage = func() {
		fmt.Fprintf(w, "Usage:\n  museum %s %s\n\nFlags:\n", cmd, usage)
		fs.PrintDefaults()
	}
	return fs
}

// museumStore opens the object storage every command reads from, and returns
// the bucket it should use.
//
// Connection settings come from the environment rather than from flags so that
// one set of variables configures every subcommand identically, whether it runs
// from a shell, a container or a scheduler.
func museumStore() (*storage.S3Service[models.Museum], string, error) {
	env.LoadEnv()

	bucket, err := env.LookupEnv("MUSEUM_BUCKET_NAME")
	if err != nil {
		return nil, "", err
	}

	store, err := storage.NewS3Service(keys.Museum)
	if err != nil {
		return nil, "", err
	}
	return store, bucket, nil
}

// database opens the catalogue database.
//
// Like the object-storage settings, the connection string comes from the
// environment so one set of variables configures every subcommand identically.
func database(ctx context.Context) (*postgres.Store, error) {
	env.LoadEnv()

	dsn, err := env.LookupEnv("DATABASE_URL")
	if err != nil {
		return nil, err
	}
	return postgres.Open(ctx, dsn)
}

// requireNoArgs reports an error when a command was given positional arguments
// it does not take, rather than ignoring them silently.
func requireNoArgs(cmd string, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("%s takes no arguments, got %q", cmd, strings.Join(args, " "))
	}
	return nil
}
