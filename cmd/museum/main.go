// Command museum builds, enriches and serves a catalogue of the world's
// museums, and the exhibitions currently on show in them.
//
//	museum crawl                 build the catalogue from Wikidata/Wikipedia/OSM
//	museum enrich                consume storage events and geocode
//	museum refresh -all          scrape museum websites for exhibitions
//	museum reindex               rebuild the geo index from stored records
//	museum serve                 run the HTTP API
//	museum query museums -place "Paris" -radius 2
//
// Each subcommand is its own process with its own lifecycle, but they share one
// binary and one set of connection settings. They never call each other: every
// subcommand reads and writes one object-storage bucket, and coordinates
// through key prefixes alone.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"museum/internal/command"
)

func main() {
	program := filepath.Base(os.Args[0])

	flag.CommandLine.SetOutput(os.Stderr)
	flag.Usage = func() { command.Usage(os.Stderr, program) }

	args := os.Args[1:]
	if len(args) == 0 {
		command.Usage(os.Stderr, program)
		os.Exit(2)
	}

	switch name := args[0]; name {
	case "help", "-h", "--help":
		command.Usage(os.Stdout, program)
		return

	default:
		cmd, ok := command.Lookup(name)
		if !ok {
			fmt.Fprintf(os.Stderr, "%s: unknown command %q\n\n", program, name)
			command.Usage(os.Stderr, program)
			os.Exit(2)
		}

		if err := cmd.Run(context.Background(), args[1:]); err != nil {
			// A flag parse error has already been reported by the flag package,
			// and -h is a request for help rather than a failure.
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "%s %s: %v\n", program, name, err)
			os.Exit(1)
		}
	}
}
