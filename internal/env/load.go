// Package env loads configuration from a .env file and the process
// environment.
package env

import (
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/joho/godotenv"
)

// loadOnce guards against re-reading .env when several components each want to
// be sure it has been loaded.
var loadOnce sync.Once

// LoadEnv reads a .env file into the environment if one is present. A missing
// file is not an error: in a container the variables are supplied directly.
func LoadEnv() {
	loadOnce.Do(func() {
		if err := godotenv.Load(); err != nil {
			log.Println("No .env file found, assuming environment variables are set directly.")
		}
	})
}

// LookupEnv returns the value of a required variable, or an error naming it.
//
// Prefer this to MustGetEnv anywhere the caller can report a problem itself:
// a subcommand that returns an error can print one clean message, whereas
// exiting from deep inside a library skips every deferred cleanup on the way
// out.
func LookupEnv(key string) (string, error) {
	value, ok := os.LookupEnv(key)
	if !ok || value == "" {
		return "", fmt.Errorf("environment variable %s is not set", key)
	}
	return value, nil
}

// MustGetEnv returns the value of a required variable, exiting if it is unset.
func MustGetEnv(key string) string {
	value, err := LookupEnv(key)
	if err != nil {
		log.Fatal(err)
	}
	return value
}
