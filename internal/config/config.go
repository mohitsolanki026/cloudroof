// Package config resolves runtime configuration from flags, environment, and
// sensible defaults. Bosun is self-hosted and expected to run with zero
// configuration, so every value here has a working default.
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	// Addr is the listen address for the HTTP API and UI.
	Addr string
	// DataDir holds the SQLite database and the master key file.
	DataDir string
	// MasterKeyPath is the file containing the 32-byte secret-sealing key.
	// Generated on first run if absent.
	MasterKeyPath string
	// MasterKeyEnv, when set, overrides the key file. Intended for container
	// users who inject secrets as environment variables.
	MasterKeyEnv string
	// Dev serves the frontend from disk instead of the embedded bundle.
	Dev bool
}

// DBPath is the SQLite database file.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "bosun.db") }

// Load parses flags and environment into a Config, creating the data directory
// if it does not exist.
func Load(args []string) (Config, error) {
	fs := flag.NewFlagSet("bosun", flag.ContinueOnError)

	var c Config
	fs.StringVar(&c.Addr, "addr", envOr("BOSUN_ADDR", ":7070"), "listen address")
	fs.StringVar(&c.DataDir, "data", envOr("BOSUN_DATA", defaultDataDir()), "data directory")
	fs.BoolVar(&c.Dev, "dev", os.Getenv("BOSUN_DEV") != "", "serve frontend from ./web/dist instead of embedded bundle")

	if err := fs.Parse(args); err != nil {
		return c, err
	}

	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return c, fmt.Errorf("resolve data dir: %w", err)
	}
	c.DataDir = abs
	c.MasterKeyPath = filepath.Join(c.DataDir, "master.key")
	c.MasterKeyEnv = os.Getenv("BOSUN_MASTER_KEY")

	// 0700: the data directory holds sealed SSH keys and the master key beside
	// each other. Group and world have no business here.
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return c, fmt.Errorf("create data dir %s: %w", c.DataDir, err)
	}
	return c, nil
}

func defaultDataDir() string {
	// Prefer XDG, fall back to a dot-directory, fall back to the working
	// directory. The last case covers containers running without HOME set.
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "bosun")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".bosun")
	}
	return "./bosun-data"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
